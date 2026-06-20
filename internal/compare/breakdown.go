package compare

import (
	"sort"
	"strings"

	"github.com/TubbyStubby/mycelia/internal/v8profile"
)

// maxStitchDepth bounds how far the caller walk climbs through transparent
// frames, guarding against pathological or cyclic transparent chains.
const maxStitchDepth = 16

// transparentFrameNames are runtime/trampoline frames whose own caller is the
// meaningful one. When stitching, the breakdown walks through them to the
// nearest real frame. Matched by function name (the leading token of Display).
//
// V8 CPU profiles only record the synchronous sampled stack, so across an await
// the logical caller is replaced by the microtask runner or a library
// trampoline. These are the ones seen dead-ending real drilldowns.
var transparentFrameNames = map[string]bool{
	"runMicrotasks":          true,
	"(garbage collector)":    true,
	"(program)":              true,
	"ErrorPrepareStackTrace": true,
}

// transparentByPackage scopes library trampolines to their package, so a user
// function coincidentally named "wrap" is not collapsed.
var transparentByPackage = map[string]map[string]bool{
	"kareem": {"syncWrapper": true, "wrap": true},
}

// BreakdownEdge is one caller or callee of a function, with its summed inclusive
// contribution averaged per profile (consistent with the rest of the matrix).
// ViaAsync marks a caller reached by stitching through a transparent frame, so
// the attribution is honest about the hop.
type BreakdownEdge struct {
	Key          string  `json:"key"`
	Display      string  `json:"display"`
	Package      string  `json:"package,omitempty"`
	TotalMicros  float64 `json:"totalMicros"`
	TotalSamples float64 `json:"totalSamples"`
	// SelfMicros/SelfSamples are populated only for membership sections (the
	// functions/files of a package or file, where self is the figure that
	// partitions cleanly), and for context package/file slices (where Self ==
	// Total). They are zero (omitted) for caller/callee/context edges, whose
	// figure is inclusive Total.
	SelfMicros  float64 `json:"selfMicros,omitempty"`
	SelfSamples float64 `json:"selfSamples,omitempty"`
	ViaAsync    bool    `json:"viaAsync,omitempty"`

	// PctOfFunction is set only on a function's context edges (Breakdown.Contexts
	// of a function): this context's share of the function's total inclusive time
	// — the contexts sum to ~100%, modulo unattributed samples.
	//
	// PctOfContext is set on any edge that pairs an entity with a context (a
	// function's contexts, and the functions/packages/files of a context, and a
	// package's/file's contexts): the entity's CPU under the route as a share of
	// the route's own total CPU. A high value means the entity accounts for a
	// large fraction of that route's cost, so optimizing it saves the route
	// proportionally more. Zero (omitted) for plain caller/callee/membership edges.
	PctOfFunction float64 `json:"pctOfFunction,omitempty"`
	PctOfContext  float64 `json:"pctOfContext,omitempty"`
}

// ContextSort selects how Breakdown.Contexts is ordered before the topN cap.
type ContextSort string

const (
	// CtxSortMicros ranks contexts by absolute inclusive time (the default).
	CtxSortMicros ContextSort = "micros"
	// CtxSortPctOfContext ranks contexts by the function's share of each route's
	// own busy CPU, surfacing the routes the function accounts for most of first.
	CtxSortPctOfContext ContextSort = "pctOfContext"
)

// ContextSorts lists the valid context orderings, for input validation.
var ContextSorts = []ContextSort{CtxSortMicros, CtxSortPctOfContext}

// Breakdown is a function's immediate callers and callees within one group,
// ranked by inclusive cost. It turns a hot function's inclusive time into "where
// it comes from" (callers) and "where it goes" (callees).
type Breakdown struct {
	// Dimension records which kind of entity was drilled (function/package/file/
	// context), so a client knows which sections to expect. Empty is treated as
	// function for backward compatibility.
	Dimension Dimension       `json:"dimension,omitempty"`
	Key       string          `json:"key"`
	Display   string          `json:"display"`
	Package   string          `json:"package,omitempty"`
	Callers   []BreakdownEdge `json:"callers,omitempty"`
	Callees   []BreakdownEdge `json:"callees,omitempty"`
	// Contexts is the distribution of logical owners (route/job labels) over this
	// entity's time, when the profiles carried async-context data. Empty
	// otherwise. For a function it answers "which route drives F" with real
	// causality rather than the stitched approximation in Callers; for a package
	// or file it answers "which routes exercise this code".
	Contexts []BreakdownEdge `json:"contexts,omitempty"`
	// Functions/Files/Packages are membership / composition sections for the
	// non-function dimensions: a package's functions and files, a file's
	// functions, and a context's functions, packages, and files. Empty for a
	// function breakdown (which uses Callers/Callees instead).
	Functions []BreakdownEdge `json:"functions,omitempty"`
	Files     []BreakdownEdge `json:"files,omitempty"`
	Packages  []BreakdownEdge `json:"packages,omitempty"`
}

// BuildBreakdown assembles the caller/callee breakdown of fnKey from a group's
// aggregation, returning ok=false when the function is absent. Edges are
// per-profile averaged and ranked by inclusive micros, capped at topN (0 = all).
//
// When stitch is set, callers that are transparent trampolines (async/native
// frames, see transparentFrameNames) are skipped: their cost is attributed up to
// the nearest meaningful ancestor, proportional to that ancestor's share of the
// trampoline's inbound edges, and the resulting edge is marked ViaAsync. This
// only re-attributes caller edges; callees, totals, and ranking are unaffected.
func BuildBreakdown(agg *v8profile.Aggregation, fnKey string, topN int, stitch bool, ctxSort ContextSort) (Breakdown, bool) {
	fn := agg.Functions[fnKey]
	if fn == nil {
		return Breakdown{}, false
	}
	pc := profileCount(agg)
	bd := Breakdown{Dimension: DimFunction, Key: fnKey, Display: fn.Display, Package: fn.Package}

	// Callees: the row keyed directly by this function (never stitched).
	for callee, m := range agg.Edges[fnKey] {
		bd.Callees = append(bd.Callees, edge(agg, callee, m, pc))
	}

	if stitch {
		bd.Callers = stitchedCallers(agg, fnKey, buildIncoming(agg.Edges), pc)
	} else {
		// Raw: every edge whose callee is this function.
		for caller, callees := range agg.Edges {
			if m, ok := callees[fnKey]; ok {
				bd.Callers = append(bd.Callers, edge(agg, caller, m, pc))
			}
		}
	}

	// Contexts: inclusive owners of this function (route/job), exact (not stitched).
	// The two percentages are ratios, so they're taken from the summed values
	// (averaging-invariant): the function's own inclusive total and each context's
	// total busy CPU. Both are already in the aggregation.
	for label, m := range agg.FunctionContexts[fnKey] {
		ce := edge(agg, label, m, pc)
		ce.PctOfFunction = pct(m.TotalMicros, fn.Metric.TotalMicros)
		if cx := agg.Contexts[label]; cx != nil {
			ce.PctOfContext = pct(m.TotalMicros, cx.Metric.TotalMicros)
		}
		bd.Contexts = append(bd.Contexts, ce)
	}

	bd.Callers = rankEdges(bd.Callers, topN)
	bd.Callees = rankEdges(bd.Callees, topN)
	bd.Contexts = rankContexts(bd.Contexts, topN, ctxSort)
	return bd, true
}

// BuildEntityBreakdown is the dimension-aware entry point. For a function it
// delegates to BuildBreakdown (callers/callees/contexts). For a package, file,
// or context it returns the relevant membership / composition sections instead.
// ok is false when key is absent from that dimension's entity set. stitch and
// ctxSort apply only to the function path.
func BuildEntityBreakdown(agg *v8profile.Aggregation, dim Dimension, key string, topN int, stitch bool, ctxSort ContextSort) (Breakdown, bool) {
	switch dim {
	case DimPackage:
		return buildPackageBreakdown(agg, key, topN)
	case DimFile:
		return buildFileBreakdown(agg, key, topN)
	case DimContext:
		return buildContextBreakdown(agg, key, topN)
	default:
		return BuildBreakdown(agg, key, topN, stitch, ctxSort)
	}
}

// buildPackageBreakdown lists a package's member functions and files (by self
// time, which partitions cleanly) and the contexts that exercise it.
func buildPackageBreakdown(agg *v8profile.Aggregation, key string, topN int) (Breakdown, bool) {
	pkg := agg.Packages[key]
	if pkg == nil {
		return Breakdown{}, false
	}
	pc := profileCount(agg)
	bd := Breakdown{Dimension: DimPackage, Key: key, Display: pkg.Display}
	for _, f := range agg.Functions {
		if f.Package == key {
			bd.Functions = append(bd.Functions, memberEdge(f, pc))
		}
	}
	for _, fl := range agg.Files {
		if fl.Package == key {
			bd.Files = append(bd.Files, memberEdge(fl, pc))
		}
	}
	bd.Contexts = contextsForEntity(agg, agg.ContextPackages, key, pc)
	bd.Functions = rankBySelf(bd.Functions, topN)
	bd.Files = rankBySelf(bd.Files, topN)
	bd.Contexts = rankBySelf(bd.Contexts, topN)
	return bd, true
}

// buildFileBreakdown lists a file's member functions (by self time) and the
// contexts that exercise it. File membership is exact via Entity.File.
func buildFileBreakdown(agg *v8profile.Aggregation, key string, topN int) (Breakdown, bool) {
	file := agg.Files[key]
	if file == nil {
		return Breakdown{}, false
	}
	pc := profileCount(agg)
	bd := Breakdown{Dimension: DimFile, Key: key, Display: file.Display, Package: file.Package}
	for _, f := range agg.Functions {
		if f.File == key {
			bd.Functions = append(bd.Functions, memberEdge(f, pc))
		}
	}
	bd.Contexts = contextsForEntity(agg, agg.ContextFiles, key, pc)
	bd.Functions = rankBySelf(bd.Functions, topN)
	bd.Contexts = rankBySelf(bd.Contexts, topN)
	return bd, true
}

// buildContextBreakdown decomposes a context (route/job): the functions running
// under it (inclusive, from FunctionContexts) and the packages/files its self
// time lands in (which sum to the context total).
func buildContextBreakdown(agg *v8profile.Aggregation, key string, topN int) (Breakdown, bool) {
	cx := agg.Contexts[key]
	if cx == nil {
		return Breakdown{}, false
	}
	pc := profileCount(agg)
	ctxTotal := cx.Metric.TotalMicros
	bd := Breakdown{Dimension: DimContext, Key: key, Display: cx.Display}
	for fnKey, row := range agg.FunctionContexts {
		m, ok := row[key]
		if !ok {
			continue
		}
		e := edge(agg, fnKey, m, pc) // inclusive Total
		e.PctOfContext = pct(m.TotalMicros, ctxTotal)
		bd.Functions = append(bd.Functions, e)
	}
	bd.Packages = entitiesOfContext(agg, agg.ContextPackages[key], ctxTotal, pc, false)
	bd.Files = entitiesOfContext(agg, agg.ContextFiles[key], ctxTotal, pc, true)
	bd.Functions = rankEdges(bd.Functions, topN) // by inclusive Total
	bd.Packages = rankBySelf(bd.Packages, topN)
	bd.Files = rankBySelf(bd.Files, topN)
	return bd, true
}

// contextsForEntity builds the contexts section for a package or file: for each
// label, the entity's self CPU under it (from ctxMap), with pctOfContext = that
// entity's share of the route's own total. ctxMap may be nil (no async data).
func contextsForEntity(agg *v8profile.Aggregation, ctxMap map[string]map[string]v8profile.Metric, entityKey string, pc int) []BreakdownEdge {
	var out []BreakdownEdge
	for label, row := range ctxMap {
		m, ok := row[entityKey]
		if !ok {
			continue
		}
		e := metricEdge(label, label, "", m, pc)
		if cx := agg.Contexts[label]; cx != nil {
			e.PctOfContext = pct(m.SelfMicros, cx.Metric.TotalMicros)
		}
		out = append(out, e)
	}
	return out
}

// entitiesOfContext builds the package or file slices of one context: each key's
// self CPU under the context, with pctOfContext = its share of the context total.
// When withFilePkg, the file's package is attached for display. row may be nil.
func entitiesOfContext(agg *v8profile.Aggregation, row map[string]v8profile.Metric, ctxTotal int64, pc int, withFilePkg bool) []BreakdownEdge {
	var out []BreakdownEdge
	for key, m := range row {
		pkg := ""
		if withFilePkg {
			if fe := agg.Files[key]; fe != nil {
				pkg = fe.Package
			}
		}
		e := metricEdge(key, key, pkg, m, pc)
		e.PctOfContext = pct(m.SelfMicros, ctxTotal)
		out = append(out, e)
	}
	return out
}

// memberEdge builds an edge for a member entity (a package's function or file, or
// a file's function), carrying both self and inclusive totals, per-profile
// averaged. Membership sections rank and chart on self.
func memberEdge(e *v8profile.Entity, pc int) BreakdownEdge {
	return BreakdownEdge{
		Key: e.Key, Display: e.Display, Package: e.Package,
		SelfMicros: avg(e.Metric.SelfMicros, pc), SelfSamples: avg(e.Metric.SelfSamples, pc),
		TotalMicros: avg(e.Metric.TotalMicros, pc), TotalSamples: avg(e.Metric.TotalSamples, pc),
	}
}

// metricEdge builds an edge from a bare metric and labels, per-profile averaged.
func metricEdge(key, display, pkg string, m v8profile.Metric, pc int) BreakdownEdge {
	return BreakdownEdge{
		Key: key, Display: display, Package: pkg,
		SelfMicros: avg(m.SelfMicros, pc), SelfSamples: avg(m.SelfSamples, pc),
		TotalMicros: avg(m.TotalMicros, pc), TotalSamples: avg(m.TotalSamples, pc),
	}
}

// rankBySelf sorts edges by self micros (descending), name as tie-break, and
// caps at topN (0 = all).
func rankBySelf(edges []BreakdownEdge, topN int) []BreakdownEdge {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].SelfMicros != edges[j].SelfMicros {
			return edges[i].SelfMicros > edges[j].SelfMicros
		}
		return edges[i].Display < edges[j].Display
	})
	if topN > 0 && len(edges) > topN {
		edges = edges[:topN]
	}
	return edges
}

// edgeAcc accumulates a stitched caller's attributed cost across walk paths.
type edgeAcc struct {
	micros   float64
	samples  float64
	viaAsync bool
}

// stitchedCallers attributes fnKey's inbound edges up past transparent frames to
// the nearest meaningful ancestors. incoming maps callee -> caller -> edge.
func stitchedCallers(agg *v8profile.Aggregation, fnKey string, incoming map[string]map[string]v8profile.Metric, pc int) []BreakdownEdge {
	result := map[string]*edgeAcc{}

	settle := func(node string, micros, samples float64, hopped bool) {
		acc := result[node]
		if acc == nil {
			acc = &edgeAcc{}
			result[node] = acc
		}
		acc.micros += micros
		acc.samples += samples
		acc.viaAsync = acc.viaAsync || hopped
	}

	// add walks from node upward. visited tracks the current DFS path so a cyclic
	// transparent chain settles instead of recursing forever.
	var add func(node string, micros, samples float64, hopped bool, visited map[string]bool, depth int)
	add = func(node string, micros, samples float64, hopped bool, visited map[string]bool, depth int) {
		ins := incoming[node]
		if !isTransparentFrame(agg.Functions[node]) || depth >= maxStitchDepth || visited[node] || len(ins) == 0 {
			settle(node, micros, samples, hopped)
			return
		}
		var totalIn float64
		for _, m := range ins {
			totalIn += float64(m.TotalMicros)
		}
		if totalIn <= 0 {
			// A top trampoline with no attributable cost above it: keep the cost
			// here rather than losing it.
			settle(node, micros, samples, hopped)
			return
		}
		visited[node] = true
		for parent, m := range ins {
			ratio := float64(m.TotalMicros) / totalIn
			add(parent, micros*ratio, samples*ratio, true, visited, depth+1)
		}
		delete(visited, node)
	}

	for caller, m := range incoming[fnKey] {
		add(caller, float64(m.TotalMicros), float64(m.TotalSamples), false, map[string]bool{}, 0)
	}

	out := make([]BreakdownEdge, 0, len(result))
	for key, acc := range result {
		display, pkg := key, ""
		if e := agg.Functions[key]; e != nil {
			display, pkg = e.Display, e.Package
		}
		out = append(out, BreakdownEdge{
			Key:          key,
			Display:      display,
			Package:      pkg,
			TotalMicros:  acc.micros / float64(pc),
			TotalSamples: acc.samples / float64(pc),
			ViaAsync:     acc.viaAsync,
		})
	}
	return out
}

// buildIncoming inverts the caller->callee edge map into callee->caller->metric.
func buildIncoming(edges map[string]map[string]v8profile.Metric) map[string]map[string]v8profile.Metric {
	incoming := make(map[string]map[string]v8profile.Metric, len(edges))
	for caller, callees := range edges {
		for callee, m := range callees {
			row := incoming[callee]
			if row == nil {
				row = make(map[string]v8profile.Metric)
				incoming[callee] = row
			}
			row[caller] = m // each (caller,callee) pair is unique in edges
		}
	}
	return incoming
}

// isTransparentFrame reports whether an entity is a runtime/trampoline frame to
// stitch through when resolving breakdown callers.
func isTransparentFrame(e *v8profile.Entity) bool {
	if e == nil {
		return false
	}
	name := frameName(e.Display)
	if transparentFrameNames[name] {
		return true
	}
	if m := transparentByPackage[e.Package]; m != nil && m[name] {
		return true
	}
	return false
}

// frameName extracts a function's bare name from its display label, which is
// "name (url:line)" for source frames and just "name" for native ones.
func frameName(display string) string {
	if name, _, found := strings.Cut(display, " ("); found {
		return name
	}
	return display
}

// edge resolves a function key to a labelled, per-profile-averaged edge.
func edge(agg *v8profile.Aggregation, key string, m v8profile.Metric, pc int) BreakdownEdge {
	display, pkg := key, ""
	if e := agg.Functions[key]; e != nil {
		display, pkg = e.Display, e.Package
	}
	return BreakdownEdge{
		Key:          key,
		Display:      display,
		Package:      pkg,
		TotalMicros:  avg(m.TotalMicros, pc),
		TotalSamples: avg(m.TotalSamples, pc),
	}
}

// rankContexts orders context edges and caps at topN. The default ranks by
// absolute inclusive micros (same as rankEdges); CtxSortPctOfContext ranks by
// the function's share of each route's own CPU, with micros then name as
// tie-breakers so the order is deterministic.
func rankContexts(edges []BreakdownEdge, topN int, sortBy ContextSort) []BreakdownEdge {
	if sortBy != CtxSortPctOfContext {
		return rankEdges(edges, topN)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].PctOfContext != edges[j].PctOfContext {
			return edges[i].PctOfContext > edges[j].PctOfContext
		}
		if edges[i].TotalMicros != edges[j].TotalMicros {
			return edges[i].TotalMicros > edges[j].TotalMicros
		}
		return edges[i].Display < edges[j].Display
	})
	if topN > 0 && len(edges) > topN {
		edges = edges[:topN]
	}
	return edges
}

// rankEdges sorts edges by inclusive micros (descending) and caps at topN.
func rankEdges(edges []BreakdownEdge, topN int) []BreakdownEdge {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].TotalMicros != edges[j].TotalMicros {
			return edges[i].TotalMicros > edges[j].TotalMicros
		}
		return edges[i].Display < edges[j].Display
	})
	if topN > 0 && len(edges) > topN {
		edges = edges[:topN]
	}
	return edges
}
