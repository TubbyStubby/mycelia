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
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/TubbyStubby/mycelia/internal/cache"
	"github.com/TubbyStubby/mycelia/internal/compare"
	"github.com/TubbyStubby/mycelia/internal/config"
	"github.com/TubbyStubby/mycelia/internal/engine"
	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/store"
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
		log.Printf("per-object cache persisting to %s", cfg.CacheDir)
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
			"list dates, and env+service+date to list buildTags (leaf groups, with " +
			"their profile counts). Uploaded profiles appear under the 'upload' env. " +
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
			"metric is one of selfMicros|totalMicros|selfSamples|totalSamples|selfPct|totalPct " +
			"(default selfMicros). categories optionally filters frames to any of " +
			"native|node_modules|user|idle (default: all). topN caps rows (default 25, max 100). " +
			"Values are per-profile averages so groups of different sizes compare fairly.",
	}, getGroupHandler(eng))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "compare_groups",
		Title:       "Compare profile groups",
		Annotations: readOnly,
		Description: "Compare two or more profile groups side by side, returning per-group " +
			"summaries and a ranked table of the top entities aligned across groups (with a " +
			"per-group trend value for sparklines). Pass groups as a list of " +
			"env/service/date/buildTag identifiers (from browse_profiles). " +
			"dimension/metric/categories/topN behave as in get_group. Use this to spot " +
			"regressions or improvements between dates or build tags.",
	}, compareHandler(eng))
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

func browseHandler(eng *engine.Engine) mcp.ToolHandlerFor[browseInput, store.BrowseResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in browseInput) (*mcp.CallToolResult, store.BrowseResult, error) {
		res, err := eng.Browse(ctx, profiles.GroupFilter{
			Env: in.Env, Service: in.Service, Date: in.Date,
		}, true)
		if err != nil {
			return nil, store.BrowseResult{}, err
		}
		return nil, res, nil
	}
}

// --- get_group ---

type getGroupInput struct {
	groupRef
	Dimension  string   `json:"dimension,omitempty" jsonschema:"one of overall|package|function|file (default function)"`
	Metric     string   `json:"metric,omitempty" jsonschema:"one of selfMicros|totalMicros|selfSamples|totalSamples|selfPct|totalPct (default selfMicros)"`
	TopN       int      `json:"topN,omitempty" jsonschema:"max rows to return (default 25, max 100)"`
	Categories []string `json:"categories,omitempty" jsonschema:"filter frames to any of native|node_modules|user|idle (default: all)"`
}

// groupView is the get_group result: one group's summary plus its ranked rows.
type groupView struct {
	Summary   compare.GroupSummary `json:"summary"`
	Dimension compare.Dimension    `json:"dimension"`
	Metric    compare.Metric       `json:"metric"`
	Rows      []compare.Row        `json:"rows"`
}

func getGroupHandler(eng *engine.Engine) mcp.ToolHandlerFor[getGroupInput, groupView] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in getGroupInput) (*mcp.CallToolResult, groupView, error) {
		m, err := eng.Compare(ctx, []profiles.GroupID{in.id()},
			compare.Dimension(in.Dimension), compare.Metric(in.Metric),
			clampTopN(in.TopN), in.Categories, nil)
		if err != nil {
			return nil, groupView{}, err
		}
		view := groupView{Dimension: m.Dimension, Metric: m.Metric, Rows: m.Rows}
		if len(m.Summaries) > 0 {
			view.Summary = m.Summaries[0]
		}
		return nil, view, nil
	}
}

// --- compare_groups ---

type compareInput struct {
	Groups     []groupRef `json:"groups" jsonschema:"two or more groups to compare, by env/service/date/buildTag"`
	Dimension  string     `json:"dimension,omitempty" jsonschema:"one of overall|package|function|file (default function)"`
	Metric     string     `json:"metric,omitempty" jsonschema:"one of selfMicros|totalMicros|selfSamples|totalSamples|selfPct|totalPct (default selfMicros)"`
	TopN       int        `json:"topN,omitempty" jsonschema:"max rows to return (default 25, max 100)"`
	Categories []string   `json:"categories,omitempty" jsonschema:"filter frames to any of native|node_modules|user|idle (default: all)"`
}

func compareHandler(eng *engine.Engine) mcp.ToolHandlerFor[compareInput, compare.Matrix] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in compareInput) (*mcp.CallToolResult, compare.Matrix, error) {
		ids := make([]profiles.GroupID, len(in.Groups))
		for i, g := range in.Groups {
			ids[i] = g.id()
		}
		m, err := eng.Compare(ctx, ids,
			compare.Dimension(in.Dimension), compare.Metric(in.Metric),
			clampTopN(in.TopN), in.Categories, nil)
		if err != nil {
			return nil, compare.Matrix{}, err
		}
		return nil, m, nil
	}
}

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
