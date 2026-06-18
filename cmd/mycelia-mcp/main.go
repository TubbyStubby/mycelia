// Command mycelia-mcp exposes mycelia's V8 CPU profile data to MCP clients
// (e.g. Claude Code / Claude Desktop) over stdio. It reuses the same GCS
// configuration as the web server and serves read-only browse/inspect/compare
// tools backed by the shared engine.
//
// Configuration mirrors the mycelia web server: -bucket, -key, -root, -sample,
// -fetch-concurrency, and -cache-dir (plus their env-var equivalents). The
// -addr flag is accepted but ignored.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/TubbyStubby/mycelia/internal/cache"
	"github.com/TubbyStubby/mycelia/internal/compare"
	"github.com/TubbyStubby/mycelia/internal/config"
	"github.com/TubbyStubby/mycelia/internal/engine"
	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/store"
	"github.com/TubbyStubby/mycelia/internal/v8profile"
)

// defaultTopN caps how many ranked rows a tool returns by default; maxTopN is
// the hard ceiling so a large surface can't overflow the MCP result-size limit.
const (
	defaultTopN = 25
	maxTopN     = 100
)

func main() {
	// stdio uses stdout for protocol frames; all logging must go to stderr.
	log.SetOutput(os.Stderr)

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatalf("config: %v", err)
	}

	ctx := context.Background()

	var gcs *store.GCSSource
	if cfg.GCSEnabled() {
		gcs, err = store.NewGCSSource(ctx, cfg.Bucket, cfg.KeyFile, cfg.RootPath)
		if err != nil {
			log.Fatalf("gcs: %v", err)
		}
		defer gcs.Close()
		log.Printf("GCS source ready: bucket=%s root=%q", cfg.Bucket, cfg.RootPath)
	} else {
		log.Printf("GCS not configured (need -bucket and -key); upload-only mode (no profiles available over MCP)")
	}

	uploads := store.NewUploadSource()
	objCache, err := cache.NewObjectCache(cfg.CacheDir)
	if err != nil {
		log.Fatalf("object cache: %v", err)
	}
	if cfg.CacheDir != "" {
		log.Printf("per-object cache persisting to %s", filepath.Join(cfg.CacheDir, cache.VersionDir()))
	}
	eng := engine.New(cfg, gcs, uploads, cache.New(), objCache)

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "mycelia",
		Title:   "Mycelia CPU Profiles",
		Version: "0.1.0",
	}, nil)
	registerTools(srv, eng)

	log.Printf("mycelia-mcp serving over stdio")
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("mcp server: %v", err)
	}
}

// readOnly marks a tool as non-mutating for the host UI.
var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true}

func registerTools(s *mcp.Server, eng *engine.Engine) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browse_profiles",
		Title:       "Browse profile groups",
		Annotations: readOnly,
		Description: "Discover available CPU profile groups by drilling down the " +
			"env -> service -> date -> buildTag hierarchy. Call with no arguments to " +
			"list environments, then pass env to list its services, env+service to " +
			"list dates, and env+service+date to list buildTags (leaf groups, each " +
			"with its profileCount and the firstTs/lastTs wall-clock span of its " +
			"profiles). Uploaded profiles appear under the 'upload' env. " +
			"Use the returned identifiers with get_group or compare_groups.",
	}, browseHandler(eng))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_group",
		Title:       "Inspect a profile group",
		Annotations: readOnly,
		Description: "Return a single group's headline metrics and its top hotspots " +
			"for one dimension, ranked by a metric. Identify the group by " +
			"env/service/date/buildTag (from browse_profiles). " +
			"dimension is one of overall|package|function|file (default function). " +
			"metric is one of selfMicros|totalMicros|selfSamples|totalSamples|selfPct|totalPct|" +
			"selfPctBusy|totalPctBusy (default selfMicros); the *PctBusy variants are shares of " +
			"non-idle CPU, so they compare composition independent of load. categories optionally " +
			"filters frames to any of native|node_modules|user|idle (default: all). topN caps rows " +
			"(default 25, max 100). Optional from/to (RFC3339) restrict to profiles in that time " +
			"window. The summary reports idlePct/busyPct and the merged firstTs/lastTs. " +
			"Values are per-profile averages so groups of different sizes compare fairly. Groups " +
			"larger than the server's sample size (default 40) are deterministically sampled; " +
			"summary.profileCount vs summary.totalProfiles shows how many were merged.",
	}, getGroupHandler(eng))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "compare_groups",
		Title:       "Compare profile groups",
		Annotations: readOnly,
		Description: "Compare two or more profile groups side by side, returning per-group " +
			"summaries and a ranked table of the top entities aligned across groups. Each row " +
			"carries per-group cells plus delta/deltaPct (last group vs first). Pass groups as a " +
			"list of env/service/date/buildTag identifiers (from browse_profiles), ordered baseline " +
			"first. dimension/metric/categories/topN/from/to behave as in get_group. sort is one of " +
			"max (default, rank by largest value) | delta (rank by absolute change) | deltaPct " +
			"(rank by relative change; newly-appeared entities rank first) — use delta/deltaPct to " +
			"surface regressions between two builds. Groups larger than the sample size (default 40) " +
			"are deterministically sampled.",
	}, compareHandler(eng))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_function_breakdown",
		Title:       "Break down a function's call graph",
		Annotations: readOnly,
		Description: "Given a hot function, return its immediate callers (where its inclusive " +
			"time comes from) and callees (where that time goes), ranked by inclusive micros. " +
			"Identify the group by env/service/date/buildTag and pass function as the key field " +
			"of a row from get_group/compare_groups (the dimension must have been 'function'). " +
			"topN caps callers and callees each (default 25, max 100). Values are per-profile " +
			"averages. Use this to root-cause a hot path without leaving the profile.",
	}, breakdownHandler(eng))
}

// groupRef identifies a profile group. Mirrors profiles.GroupID with schema docs.
type groupRef struct {
	Env      string `json:"env" jsonschema:"environment name, e.g. prod (use 'upload' for uploaded profiles)"`
	Service  string `json:"service" jsonschema:"service name"`
	Date     string `json:"date" jsonschema:"group date as YYYY-MM-DD (UTC)"`
	BuildTag string `json:"buildTag" jsonschema:"build tag identifying the deploy"`
}

func (g groupRef) id() profiles.GroupID {
	return profiles.GroupID{Env: g.Env, Service: g.Service, Date: g.Date, BuildTag: g.BuildTag}
}

// --- browse_profiles ---

type browseInput struct {
	Env     string `json:"env,omitempty" jsonschema:"environment to drill into; omit to list environments"`
	Service string `json:"service,omitempty" jsonschema:"service to drill into; requires env"`
	Date    string `json:"date,omitempty" jsonschema:"date (YYYY-MM-DD) to drill into; requires env and service"`
}

// browseResult is the slimmed browse payload for MCP. At the leaf (buildTag)
// level it returns one compact row per group instead of the full member list,
// which would otherwise blow the tool-output token budget.
type browseResult struct {
	Level    store.Level  `json:"level"`
	Children []string     `json:"children,omitempty"`
	Groups   []browseLeaf `json:"groups,omitempty"`
}

// browseLeaf identifies a leaf group by its buildTag and summarizes it without
// member-level detail. FirstTs/LastTs bound the profiles' wall-clock span.
type browseLeaf struct {
	BuildTag     string `json:"buildTag"`
	ProfileCount int    `json:"profileCount"`
	FirstTs      string `json:"firstTs,omitempty"`
	LastTs       string `json:"lastTs,omitempty"`
}

func browseHandler(eng *engine.Engine) mcp.ToolHandlerFor[browseInput, browseResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in browseInput) (*mcp.CallToolResult, browseResult, error) {
		res, err := eng.Browse(ctx, profiles.GroupFilter{
			Env: in.Env, Service: in.Service, Date: in.Date,
		}, true)
		if err != nil {
			return nil, browseResult{}, err
		}
		out := browseResult{Level: res.Level, Children: res.Children}
		for _, g := range res.Groups {
			first, last := memberSpan(g.Members)
			out.Groups = append(out.Groups, browseLeaf{
				BuildTag:     g.ID.BuildTag,
				ProfileCount: len(g.Members),
				FirstTs:      formatTs(first),
				LastTs:       formatTs(last),
			})
		}
		return nil, out, nil
	}
}

// --- get_group ---

type getGroupInput struct {
	groupRef
	Dimension  string   `json:"dimension,omitempty" jsonschema:"one of overall|package|function|file (default function)"`
	Metric     string   `json:"metric,omitempty" jsonschema:"one of selfMicros|totalMicros|selfSamples|totalSamples|selfPct|totalPct|selfPctBusy|totalPctBusy (default selfMicros)"`
	TopN       int      `json:"topN,omitempty" jsonschema:"max rows to return (default 25, max 100)"`
	Categories []string `json:"categories,omitempty" jsonschema:"filter frames to any of native|node_modules|user|idle (default: all)"`
	Sort       string   `json:"sort,omitempty" jsonschema:"row ranking: max (default) | delta | deltaPct"`
	From       string   `json:"from,omitempty" jsonschema:"only include profiles at/after this RFC3339 timestamp"`
	To         string   `json:"to,omitempty" jsonschema:"only include profiles at/before this RFC3339 timestamp"`
}

// groupView is the get_group result: one group's summary plus its ranked rows.
type groupView struct {
	Summary   mcpSummary        `json:"summary"`
	Dimension compare.Dimension `json:"dimension"`
	Metric    compare.Metric    `json:"metric"`
	Sort      compare.SortMode  `json:"sort"`
	Rows      []mcpRow          `json:"rows"`
}

func getGroupHandler(eng *engine.Engine) mcp.ToolHandlerFor[getGroupInput, groupView] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in getGroupInput) (*mcp.CallToolResult, groupView, error) {
		opts, err := buildOptions(in.Dimension, in.Metric, in.Sort, in.From, in.To, in.TopN, in.Categories)
		if err != nil {
			return nil, groupView{}, err
		}
		m, err := eng.Compare(ctx, []profiles.GroupID{in.id()}, opts, nil)
		if err != nil {
			return nil, groupView{}, err
		}
		view := groupView{
			Dimension: m.Dimension, Metric: m.Metric, Sort: effectiveSort(opts.Sort),
			Rows: toMCPRows(m.Rows),
		}
		if len(m.Summaries) > 0 {
			view.Summary = toMCPSummary(m.Summaries[0])
		}
		return nil, view, nil
	}
}

// --- compare_groups ---

type compareInput struct {
	Groups     []groupRef `json:"groups" jsonschema:"two or more groups to compare (baseline first), by env/service/date/buildTag"`
	Dimension  string     `json:"dimension,omitempty" jsonschema:"one of overall|package|function|file (default function)"`
	Metric     string     `json:"metric,omitempty" jsonschema:"one of selfMicros|totalMicros|selfSamples|totalSamples|selfPct|totalPct|selfPctBusy|totalPctBusy (default selfMicros)"`
	TopN       int        `json:"topN,omitempty" jsonschema:"max rows to return (default 25, max 100)"`
	Categories []string   `json:"categories,omitempty" jsonschema:"filter frames to any of native|node_modules|user|idle (default: all)"`
	Sort       string     `json:"sort,omitempty" jsonschema:"row ranking: max (default) | delta | deltaPct"`
	From       string     `json:"from,omitempty" jsonschema:"only include profiles at/after this RFC3339 timestamp"`
	To         string     `json:"to,omitempty" jsonschema:"only include profiles at/before this RFC3339 timestamp"`
}

// compareView is the compare_groups result: a rounded, trend-free matrix.
type compareView struct {
	Dimension compare.Dimension  `json:"dimension"`
	Metric    compare.Metric     `json:"metric"`
	Sort      compare.SortMode   `json:"sort"`
	Groups    []profiles.GroupID `json:"groups"`
	Summaries []mcpSummary       `json:"summaries"`
	Rows      []mcpRow           `json:"rows"`
}

func compareHandler(eng *engine.Engine) mcp.ToolHandlerFor[compareInput, compareView] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in compareInput) (*mcp.CallToolResult, compareView, error) {
		opts, err := buildOptions(in.Dimension, in.Metric, in.Sort, in.From, in.To, in.TopN, in.Categories)
		if err != nil {
			return nil, compareView{}, err
		}
		ids := make([]profiles.GroupID, len(in.Groups))
		for i, g := range in.Groups {
			ids[i] = g.id()
		}
		m, err := eng.Compare(ctx, ids, opts, nil)
		if err != nil {
			return nil, compareView{}, err
		}
		summaries := make([]mcpSummary, len(m.Summaries))
		for i, s := range m.Summaries {
			summaries[i] = toMCPSummary(s)
		}
		return nil, compareView{
			Dimension: m.Dimension, Metric: m.Metric, Sort: effectiveSort(opts.Sort),
			Groups: m.Groups, Summaries: summaries, Rows: toMCPRows(m.Rows),
		}, nil
	}
}

// --- get_function_breakdown ---

type breakdownInput struct {
	groupRef
	Function string `json:"function" jsonschema:"the function key (a row's 'key' from get_group/compare_groups) to break down"`
	TopN     int    `json:"topN,omitempty" jsonschema:"max callers and callees each (default 25, max 100)"`
}

// breakdownView is the rounded get_function_breakdown result.
type breakdownView struct {
	Key     string         `json:"key"`
	Display string         `json:"display"`
	Package string         `json:"package,omitempty"`
	Callers []breakdownRow `json:"callers"`
	Callees []breakdownRow `json:"callees"`
}

type breakdownRow struct {
	Key          string  `json:"key"`
	Display      string  `json:"display"`
	Package      string  `json:"package,omitempty"`
	TotalMicros  float64 `json:"totalMicros"`
	TotalSamples float64 `json:"totalSamples"`
}

func breakdownHandler(eng *engine.Engine) mcp.ToolHandlerFor[breakdownInput, breakdownView] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in breakdownInput) (*mcp.CallToolResult, breakdownView, error) {
		if in.Function == "" {
			return nil, breakdownView{}, fmt.Errorf("function key is required")
		}
		bd, err := eng.FunctionBreakdown(ctx, in.id(), in.Function, clampTopN(in.TopN))
		if err != nil {
			return nil, breakdownView{}, err
		}
		return nil, breakdownView{
			Key:     bd.Key,
			Display: bd.Display,
			Package: bd.Package,
			Callers: toBreakdownRows(bd.Callers),
			Callees: toBreakdownRows(bd.Callees),
		}, nil
	}
}

func toBreakdownRows(edges []compare.BreakdownEdge) []breakdownRow {
	out := make([]breakdownRow, len(edges))
	for i, e := range edges {
		out[i] = breakdownRow{
			Key:          e.Key,
			Display:      e.Display,
			Package:      e.Package,
			TotalMicros:  math.Round(e.TotalMicros),
			TotalSamples: round1(e.TotalSamples),
		}
	}
	return out
}

// --- option building & validation ---

// buildOptions validates the raw tool inputs and assembles engine options. It
// returns an error (surfaced to the MCP client) on any unknown enum value or
// malformed timestamp, rather than silently coercing to a default.
func buildOptions(dim, metric, sortMode, from, to string, topN int, categories []string) (engine.CompareOptions, error) {
	if err := validateEnum("dimension", dim, dimensionValues()); err != nil {
		return engine.CompareOptions{}, err
	}
	if err := validateEnum("metric", metric, metricValues()); err != nil {
		return engine.CompareOptions{}, err
	}
	if err := validateEnum("sort", sortMode, sortValues()); err != nil {
		return engine.CompareOptions{}, err
	}
	for _, c := range categories {
		if err := validateEnum("category", c, categoryValues()); err != nil {
			return engine.CompareOptions{}, err
		}
	}
	win, err := parseWindow(from, to)
	if err != nil {
		return engine.CompareOptions{}, err
	}
	return engine.CompareOptions{
		Dimension:  compare.Dimension(dim),
		Metric:     compare.Metric(metric),
		TopN:       clampTopN(topN),
		Categories: categories,
		Sort:       compare.SortMode(sortMode),
		Window:     win,
	}, nil
}

// validateEnum returns an error listing the allowed values when v is non-empty
// and not in allowed. An empty value is valid (selects the engine default).
func validateEnum(name, v string, allowed []string) error {
	if v == "" {
		return nil
	}
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("invalid %s %q; allowed: %s", name, v, strings.Join(allowed, "|"))
}

func dimensionValues() []string {
	out := make([]string, len(compare.Dimensions))
	for i, d := range compare.Dimensions {
		out[i] = string(d)
	}
	return out
}

func metricValues() []string {
	out := make([]string, len(compare.Metrics))
	for i, m := range compare.Metrics {
		out[i] = string(m)
	}
	return out
}

func sortValues() []string {
	out := make([]string, len(compare.SortModes))
	for i, s := range compare.SortModes {
		out[i] = string(s)
	}
	return out
}

func categoryValues() []string {
	return []string{v8profile.CatNative, v8profile.CatNodeModules, v8profile.CatUser, v8profile.CatIdle}
}

// parseWindow parses optional RFC3339 from/to bounds into an engine.Window.
func parseWindow(from, to string) (engine.Window, error) {
	var w engine.Window
	if from != "" {
		t, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return engine.Window{}, fmt.Errorf("invalid from %q: want RFC3339 timestamp", from)
		}
		w.From = t
	}
	if to != "" {
		t, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return engine.Window{}, fmt.Errorf("invalid to %q: want RFC3339 timestamp", to)
		}
		w.To = t
	}
	if !w.From.IsZero() && !w.To.IsZero() && w.To.Before(w.From) {
		return engine.Window{}, fmt.Errorf("invalid window: to %q is before from %q", to, from)
	}
	return w, nil
}

// effectiveSort echoes the sort mode actually applied (defaulting to max).
func effectiveSort(s compare.SortMode) compare.SortMode {
	if s == "" {
		return compare.SortMax
	}
	return s
}

// --- rounded, trend-free MCP DTOs ---

// mcpCell mirrors compare.Cell with values rounded for token economy.
type mcpCell struct {
	SelfSamples  float64 `json:"selfSamples"`
	TotalSamples float64 `json:"totalSamples"`
	SelfMicros   float64 `json:"selfMicros"`
	TotalMicros  float64 `json:"totalMicros"`
	SelfPct      float64 `json:"selfPct"`
	TotalPct     float64 `json:"totalPct"`
	SelfPctBusy  float64 `json:"selfPctBusy"`
	TotalPctBusy float64 `json:"totalPctBusy"`
}

// mcpRow mirrors compare.Row without the Trend array (redundant with Cells).
type mcpRow struct {
	Key      string    `json:"key"`
	Display  string    `json:"display"`
	Package  string    `json:"package,omitempty"`
	Cells    []mcpCell `json:"cells"`
	Delta    float64   `json:"delta"`
	DeltaPct float64   `json:"deltaPct"`
}

// mcpSummary mirrors compare.GroupSummary with rounded values and string times.
type mcpSummary struct {
	ID                profiles.GroupID `json:"id"`
	OverallMicros     float64          `json:"overallMicros"`
	OverallSamples    float64          `json:"overallSamples"`
	SampleCount       int              `json:"sampleCount"`
	ProfileCount      int              `json:"profileCount"`
	TotalProfiles     int              `json:"totalProfiles"`
	DurationMicros    float64          `json:"durationMicros"`
	TimingApproximate bool             `json:"timingApproximate"`
	IdlePct           float64          `json:"idlePct"`
	BusyPct           float64          `json:"busyPct"`
	FirstTs           string           `json:"firstTs,omitempty"`
	LastTs            string           `json:"lastTs,omitempty"`
}

func toMCPRows(rows []compare.Row) []mcpRow {
	out := make([]mcpRow, len(rows))
	for i, r := range rows {
		cells := make([]mcpCell, len(r.Cells))
		for j, c := range r.Cells {
			cells[j] = mcpCell{
				SelfSamples:  round1(c.SelfSamples),
				TotalSamples: round1(c.TotalSamples),
				SelfMicros:   math.Round(c.SelfMicros),
				TotalMicros:  math.Round(c.TotalMicros),
				SelfPct:      round2(c.SelfPct),
				TotalPct:     round2(c.TotalPct),
				SelfPctBusy:  round2(c.SelfPctBusy),
				TotalPctBusy: round2(c.TotalPctBusy),
			}
		}
		out[i] = mcpRow{
			Key:      r.Key,
			Display:  r.Display,
			Package:  r.Package,
			Cells:    cells,
			Delta:    round2(r.Delta),
			DeltaPct: round2(r.DeltaPct),
		}
	}
	return out
}

func toMCPSummary(s compare.GroupSummary) mcpSummary {
	return mcpSummary{
		ID:                s.ID,
		OverallMicros:     math.Round(s.OverallMicros),
		OverallSamples:    round1(s.OverallSamples),
		SampleCount:       s.SampleCount,
		ProfileCount:      s.ProfileCount,
		TotalProfiles:     s.TotalProfiles,
		DurationMicros:    math.Round(s.DurationMicros),
		TimingApproximate: s.TimingApproximate,
		IdlePct:           round2(s.IdlePct),
		BusyPct:           round2(s.BusyPct),
		FirstTs:           formatTs(s.FirstTs),
		LastTs:            formatTs(s.LastTs),
	}
}

// memberSpan returns the earliest and latest member timestamps (zero if none).
func memberSpan(members []profiles.GroupMember) (first, last time.Time) {
	for _, m := range members {
		ts := m.Key.Timestamp
		if ts.IsZero() {
			continue
		}
		if first.IsZero() || ts.Before(first) {
			first = ts
		}
		if last.IsZero() || ts.After(last) {
			last = ts
		}
	}
	return first, last
}

// formatTs renders a timestamp as RFC3339, or "" when zero.
func formatTs(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// clampTopN applies the default and ceiling for a requested row cap.
func clampTopN(n int) int {
	if n <= 0 {
		return defaultTopN
	}
	if n > maxTopN {
		return maxTopN
	}
	return n
}
