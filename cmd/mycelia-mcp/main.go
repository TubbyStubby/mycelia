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
			"env/service/date/buildTag (from browse_profiles); use buildTag='latest' to " +
			"auto-select the most recent build for the env/service without browsing first. " +
			"Default metric is selfPctBusy (each entity's share of non-idle CPU) — the right " +
			"lens for optimization; absolute selfMicros swings with traffic/profile count, use " +
			"it for raw cost. " +
			"dimension is one of overall|package|function|file|context (default function); context groups by async label (route/job) when profiles carry it. " +
			"metric is one of selfMicros|totalMicros|selfSamples|totalSamples|selfPct|totalPct|" +
			"selfPctBusy|totalPctBusy (default selfPctBusy); the *PctBusy variants are shares of " +
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
			"first; use buildTag='latest' in any group to auto-select the most recent build for that " +
			"env/service (each group is resolved independently, so you can compare 'latest' of two " +
			"services or 'latest' vs a pinned build). " +
			"Default metric is selfPctBusy (each entity's share of non-idle CPU) — the right " +
			"lens for optimization; absolute selfMicros swings with traffic/profile count, use " +
			"it for raw cost. " +
			"dimension/metric/categories/topN/from/to " +
			"behave as in get_group. sort is one of max (default, rank by largest value) | delta " +
			"(rank by absolute change) | deltaPct (rank by relative change; newly-appeared entities " +
			"rank first) — use delta/deltaPct to surface regressions between two builds. Groups " +
			"larger than the sample size (default 40) are deterministically sampled.",
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
			"averages. By default (stitchAsync) callers are resolved through async/native " +
			"trampoline frames (e.g. runMicrotasks) to the nearest meaningful frame, marked " +
			"viaAsync since that attribution is proportional, not exact; set stitchAsync=false " +
			"for the raw immediate callers. When the profiles carry async-context data, a contexts " +
			"list gives the logical owners (route/job) driving this function's inclusive time by " +
			"real attribution (not stitched) — the most reliable answer to 'which route drives this'. " +
			"Each context row carries BOTH pctOfFunction (the route's share of this function's time) " +
			"AND pctOfContext (this function's share of the route's own CPU — high means optimizing " +
			"it saves the route proportionally more); you do NOT need two calls with different " +
			"contextSort to see both shares. Set contextSort=pctOfContext to rank routes by route " +
			"share instead of absolute micros. Set focus=contexts to return only the owning routes " +
			"(drops callers/callees) — the cleanest answer to 'which route owns this function'. " +
			"categories optionally filters frames to any of native|node_modules|user|idle (default: " +
			"all); context-label rows are never filtered. Use this to root-cause a hot path without " +
			"leaving the profile.",
	}, breakdownHandler(eng))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_breakdown",
		Title:       "Break down a package, file, or context",
		Annotations: readOnly,
		Description: "Drill a non-function entity within one group. Identify the group by " +
			"env/service/date/buildTag, set dimension to package | file | context, and pass " +
			"key as the matching row's 'key' from get_group/compare_groups (for that dimension). " +
			"(For functions use get_function_breakdown instead.) A package returns its member " +
			"functions and files (by self micros, which partition cleanly) plus the contexts that " +
			"exercise it; a file returns its functions and contexts; a context (route/job) returns " +
			"the functions running under it (inclusive) and the packages and files its self time " +
			"lands in (which sum to the context total) — this answers 'where does this route's CPU " +
			"go'. Rows pairing an entity with a route also carry pctOfContext (the entity's share of " +
			"that route's own CPU). topN caps each section (default 25, max 100). Context sections " +
			"require profiles captured with async-context data. Values are per-profile averages.",
	}, entityBreakdownHandler(eng))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_event_loop_blocking",
		Title:       "Find what blocks the event loop",
		Annotations: readOnly,
		Description: "Find where a Node service's event loop is blocked: the synchronous " +
			"'long tasks' (uninterrupted non-idle spans >= the server's threshold, default 50ms) " +
			"that stall the loop, attributed three ways. Identify the group by env/service/date/" +
			"buildTag (from browse_profiles); use buildTag='latest' to auto-select the most recent " +
			"build. Returns: a summary (threshold, episode count, total/ max/ per-profile blocked " +
			"time); functions — the leaf functions that ran inside long tasks, ranked by blocked " +
			"micros (which code blocks); contexts — the async labels (route/job/API) that own the " +
			"blocking, when profiles carry async-context data (which API blocks); and stalls — the " +
			"worst individual episodes, each with its duration, owning context, and full root->leaf " +
			"call stack (where exactly). This is distinct from CPU hotspots in get_group: a function " +
			"can be cheap overall yet block the loop in rare long bursts, or hot yet harmless if its " +
			"time is spread across short ticks. topN caps each list (default 25, max 100). blocked " +
			"micros are per-group totals; episodesPerProfile/blockedMicrosPerProfile are per-profile " +
			"averages; stalls are absolute worst-cases (not averaged).",
	}, blockingHandler(eng))
}

// groupRef identifies a profile group. Mirrors profiles.GroupID with schema docs.
type groupRef struct {
	Env      string `json:"env" jsonschema:"environment name, e.g. prod (use 'upload' for uploaded profiles)"`
	Service  string `json:"service" jsonschema:"service name"`
	Date     string `json:"date,omitempty" jsonschema:"group date as YYYY-MM-DD (UTC); may be omitted or 'latest' when buildTag is 'latest'"`
	BuildTag string `json:"buildTag" jsonschema:"build tag identifying the deploy; use 'latest' to auto-select the most recent build for the env/service (by newest profile)"`
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
	Dimension  string   `json:"dimension,omitempty" jsonschema:"one of overall|package|function|file|context (default function); context groups by async label (route/job) when profiles carry it"`
	Metric     string   `json:"metric,omitempty" jsonschema:"one of selfMicros|totalMicros|selfSamples|totalSamples|selfPct|totalPct|selfPctBusy|totalPctBusy (default selfPctBusy)"`
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
		// Resolve 'latest' to the concrete group id before hitting the engine
		// cache, which keys on the concrete GroupID; passing "latest" would
		// always be a cache miss and could store under the wrong key.
		id, err := eng.ResolveLatest(ctx, in.id())
		if err != nil {
			return nil, groupView{}, err
		}
		m, err := eng.Compare(ctx, []profiles.GroupID{id}, opts, nil)
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
	Dimension  string     `json:"dimension,omitempty" jsonschema:"one of overall|package|function|file|context (default function); context groups by async label (route/job) when profiles carry it"`
	Metric     string     `json:"metric,omitempty" jsonschema:"one of selfMicros|totalMicros|selfSamples|totalSamples|selfPct|totalPct|selfPctBusy|totalPctBusy (default selfPctBusy)"`
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
		// Resolve 'latest' for each group independently so callers can mix
		// "latest" with pinned builds (e.g. compare latest of two services,
		// or latest vs a specific build). Concrete ids are required before
		// Compare so the group cache keys correctly.
		ids := make([]profiles.GroupID, len(in.Groups))
		for i, g := range in.Groups {
			id, err := eng.ResolveLatest(ctx, g.id())
			if err != nil {
				return nil, compareView{}, fmt.Errorf("group %d: %w", i, err)
			}
			ids[i] = id
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
	// StitchAsync is a pointer so an omitted value defaults to true (stitch on).
	StitchAsync *bool    `json:"stitchAsync,omitempty" jsonschema:"skip async/native trampoline frames (e.g. runMicrotasks) when resolving callers, attributing up to the nearest real frame; default true. Set false for the raw immediate callers."`
	ContextSort string   `json:"contextSort,omitempty" jsonschema:"order of the contexts list: micros (default, absolute inclusive time) | pctOfContext (the function's share of each route's own CPU)"`
	Categories  []string `json:"categories,omitempty" jsonschema:"filter frames to any of native|node_modules|user|idle (default: all); context-label rows are never filtered"`
	Focus       string   `json:"focus,omitempty" jsonschema:"set to 'contexts' to return only the owning routes (drops callers/callees) — use when asking which route owns a function"`
}

// breakdownView is the rounded breakdown result, shared by get_function_breakdown
// and get_breakdown. Dimension records which entity kind was drilled; only the
// sections relevant to it are populated. Contexts is emitted before Callers/
// Callees so that "which route owns this function?" is answered first in the
// serialized JSON and is not crowded out by a large callers list when the output
// is truncated. For non-function dims Contexts is empty (omitempty) so the order
// has no visible effect. Functions/Files/Packages are the membership / composition
// sections for the package/file/context dimensions.
type breakdownView struct {
	Dimension compare.Dimension `json:"dimension,omitempty"`
	Key       string            `json:"key"`
	Display   string            `json:"display"`
	Package   string            `json:"package,omitempty"`
	// Contexts comes first so the "who owns this function?" answer appears at
	// the top of the JSON output and is not truncated behind callers/callees.
	Contexts  []breakdownRow `json:"contexts,omitempty"`
	Callers   []breakdownRow `json:"callers,omitempty"`
	Callees   []breakdownRow `json:"callees,omitempty"`
	Functions []breakdownRow `json:"functions,omitempty"`
	Files     []breakdownRow `json:"files,omitempty"`
	Packages  []breakdownRow `json:"packages,omitempty"`
}

type breakdownRow struct {
	Key          string  `json:"key"`
	Display      string  `json:"display"`
	Package      string  `json:"package,omitempty"`
	TotalMicros  float64 `json:"totalMicros"`
	TotalSamples float64 `json:"totalSamples"`
	// SelfMicros/SelfSamples are set only for membership rows (a package's or
	// file's functions/files) and a context's package/file slices, where self is
	// the partitioning figure. Omitted for caller/callee/context rows.
	SelfMicros  float64 `json:"selfMicros,omitempty"`
	SelfSamples float64 `json:"selfSamples,omitempty"`
	// ViaAsync marks a caller reached by stitching through a trampoline frame, so
	// the attribution (proportional across the trampoline's callers) is honest.
	ViaAsync bool `json:"viaAsync,omitempty"`
	// PctOfFunction is set on a function's context rows (the route's share of the
	// function). PctOfContext is set wherever an entity is paired with a route
	// (a function's contexts, a context's functions/packages/files, a package's
	// or file's contexts): the entity's share of that route's own CPU.
	PctOfFunction float64 `json:"pctOfFunction,omitempty"`
	PctOfContext  float64 `json:"pctOfContext,omitempty"`
}

func breakdownHandler(eng *engine.Engine) mcp.ToolHandlerFor[breakdownInput, breakdownView] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in breakdownInput) (*mcp.CallToolResult, breakdownView, error) {
		if in.Function == "" {
			return nil, breakdownView{}, fmt.Errorf("function key is required")
		}
		stitch := in.StitchAsync == nil || *in.StitchAsync
		ctxSort := in.ContextSort
		if ctxSort == "" {
			ctxSort = string(compare.CtxSortMicros)
		}
		if err := validateEnum("contextSort", ctxSort, contextSortValues()); err != nil {
			return nil, breakdownView{}, err
		}
		if err := validateEnum("focus", in.Focus, []string{"", "contexts"}); err != nil {
			return nil, breakdownView{}, err
		}
		for _, c := range in.Categories {
			if err := validateEnum("category", c, categoryValues()); err != nil {
				return nil, breakdownView{}, err
			}
		}
		// Resolve 'latest' before the engine call so the group cache keys on
		// the concrete GroupID.
		id, err := eng.ResolveLatest(ctx, in.id())
		if err != nil {
			return nil, breakdownView{}, err
		}
		bd, err := eng.FunctionBreakdown(ctx, id, in.Function, clampTopN(in.TopN), stitch, compare.ContextSort(ctxSort), in.Categories)
		if err != nil {
			return nil, breakdownView{}, err
		}
		view := toBreakdownView(bd)
		if in.Focus == "contexts" {
			view.Callers = nil
			view.Callees = nil
		}
		return nil, view, nil
	}
}

// --- get_breakdown (package / file / context) ---

type entityBreakdownInput struct {
	groupRef
	Dimension  string   `json:"dimension" jsonschema:"the entity kind to drill: package | file | context (use get_function_breakdown for function)"`
	Key        string   `json:"key" jsonschema:"the entity key (a row's 'key' from get_group/compare_groups for that dimension)"`
	TopN       int      `json:"topN,omitempty" jsonschema:"max rows per section (default 25, max 100)"`
	Categories []string `json:"categories,omitempty" jsonschema:"filter frames to any of native|node_modules|user|idle (default: all); context-label rows are never filtered"`
}

func entityBreakdownHandler(eng *engine.Engine) mcp.ToolHandlerFor[entityBreakdownInput, breakdownView] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in entityBreakdownInput) (*mcp.CallToolResult, breakdownView, error) {
		if in.Key == "" {
			return nil, breakdownView{}, fmt.Errorf("key is required")
		}
		// Restrict to the non-function dimensions; function has its own richer tool.
		if err := validateEnum("dimension", in.Dimension, []string{
			string(compare.DimPackage), string(compare.DimFile), string(compare.DimContext),
		}); err != nil {
			return nil, breakdownView{}, err
		}
		if in.Dimension == "" {
			return nil, breakdownView{}, fmt.Errorf("dimension is required (package | file | context)")
		}
		for _, c := range in.Categories {
			if err := validateEnum("category", c, categoryValues()); err != nil {
				return nil, breakdownView{}, err
			}
		}
		// Resolve 'latest' before the engine call so the group cache keys on
		// the concrete GroupID.
		id, err := eng.ResolveLatest(ctx, in.id())
		if err != nil {
			return nil, breakdownView{}, err
		}
		bd, err := eng.EntityBreakdown(ctx, id, compare.Dimension(in.Dimension), in.Key, clampTopN(in.TopN), true, compare.CtxSortMicros, in.Categories)
		if err != nil {
			return nil, breakdownView{}, err
		}
		return nil, toBreakdownView(bd), nil
	}
}

// --- get_event_loop_blocking ---

type blockingInput struct {
	groupRef
	TopN int `json:"topN,omitempty" jsonschema:"max rows per list — functions, contexts, stalls (default 25, max 100)"`
}

// mcpBlocking is the rounded event-loop blocking result. Micros are integers;
// the per-profile averages are rounded for token economy.
type mcpBlocking struct {
	ThresholdMicros         int64            `json:"thresholdMicros"`
	Episodes                int64            `json:"episodes"`
	BlockedMicros           int64            `json:"blockedMicros"`
	MaxEpisodeMicros        int64            `json:"maxEpisodeMicros"`
	ProfileCount            int              `json:"profileCount"`
	TotalProfiles           int              `json:"totalProfiles"`
	EpisodesPerProfile      float64          `json:"episodesPerProfile"`
	BlockedMicrosPerProfile float64          `json:"blockedMicrosPerProfile"`
	Functions               []mcpBlockingRow `json:"functions"`
	Contexts                []mcpBlockingRow `json:"contexts"`
	Stalls                  []v8profile.Stall `json:"stalls"`
}

type mcpBlockingRow struct {
	Key              string `json:"key"`
	Display          string `json:"display"`
	BlockedMicros    int64  `json:"blockedMicros"`
	Episodes         int64  `json:"episodes"`
	MaxEpisodeMicros int64  `json:"maxEpisodeMicros"`
}

func blockingHandler(eng *engine.Engine) mcp.ToolHandlerFor[blockingInput, mcpBlocking] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in blockingInput) (*mcp.CallToolResult, mcpBlocking, error) {
		// Resolve 'latest' before the engine call so the group cache keys on the
		// concrete GroupID.
		id, err := eng.ResolveLatest(ctx, in.id())
		if err != nil {
			return nil, mcpBlocking{}, err
		}
		bv, err := eng.GroupBlocking(ctx, id, clampTopN(in.TopN))
		if err != nil {
			return nil, mcpBlocking{}, err
		}
		return nil, toMCPBlocking(bv), nil
	}
}

func toMCPBlocking(bv engine.BlockingView) mcpBlocking {
	return mcpBlocking{
		ThresholdMicros:         bv.ThresholdMicros,
		Episodes:                bv.Episodes,
		BlockedMicros:           bv.BlockedMicros,
		MaxEpisodeMicros:        bv.MaxEpisodeMicros,
		ProfileCount:            bv.ProfileCount,
		TotalProfiles:           bv.TotalProfiles,
		EpisodesPerProfile:      round2(bv.EpisodesPerProfile),
		BlockedMicrosPerProfile: math.Round(bv.BlockedMicrosPerProfile),
		Functions:               toMCPBlockingRows(bv.Functions),
		Contexts:                toMCPBlockingRows(bv.Contexts),
		Stalls:                  bv.Stalls,
	}
}

func toMCPBlockingRows(rows []engine.BlockingRow) []mcpBlockingRow {
	out := make([]mcpBlockingRow, len(rows))
	for i, r := range rows {
		out[i] = mcpBlockingRow{
			Key:              r.Key,
			Display:          r.Display,
			BlockedMicros:    r.BlockedMicros,
			Episodes:         r.Episodes,
			MaxEpisodeMicros: r.MaxEpisodeMicros,
		}
	}
	return out
}

// toBreakdownView maps a compare.Breakdown to the rounded MCP view, populating
// whichever sections the dimension produced (empty sections drop out via omitempty).
func toBreakdownView(bd compare.Breakdown) breakdownView {
	return breakdownView{
		Dimension: bd.Dimension,
		Key:       bd.Key,
		Display:   bd.Display,
		Package:   bd.Package,
		Callers:   toBreakdownRows(bd.Callers),
		Callees:   toBreakdownRows(bd.Callees),
		Contexts:  toBreakdownRows(bd.Contexts),
		Functions: toBreakdownRows(bd.Functions),
		Files:     toBreakdownRows(bd.Files),
		Packages:  toBreakdownRows(bd.Packages),
	}
}

func toBreakdownRows(edges []compare.BreakdownEdge) []breakdownRow {
	out := make([]breakdownRow, len(edges))
	for i, e := range edges {
		out[i] = breakdownRow{
			Key:           e.Key,
			Display:       e.Display,
			Package:       e.Package,
			TotalMicros:   math.Round(e.TotalMicros),
			TotalSamples:  round1(e.TotalSamples),
			SelfMicros:    math.Round(e.SelfMicros),
			SelfSamples:   round1(e.SelfSamples),
			ViaAsync:      e.ViaAsync,
			PctOfFunction: round2(e.PctOfFunction),
			PctOfContext:  round2(e.PctOfContext),
		}
	}
	return out
}

// --- option building & validation ---

// buildOptions validates the raw tool inputs and assembles engine options. It
// returns an error (surfaced to the MCP client) on any unknown enum value or
// malformed timestamp, rather than silently coercing to a default.
func buildOptions(dim, metric, sortMode, from, to string, topN int, categories []string) (engine.CompareOptions, error) {
	// Default to selfPctBusy at the user-facing layer: it is a load-independent
	// share of non-idle CPU, so it compares composition across groups of different
	// sizes or traffic levels. selfMicros is still accessible by explicit request.
	if metric == "" {
		metric = string(compare.MetricSelfPctBusy)
	}
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

func contextSortValues() []string {
	out := make([]string, len(compare.ContextSorts))
	for i, s := range compare.ContextSorts {
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
