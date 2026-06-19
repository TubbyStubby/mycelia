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
	ViaAsync     bool    `json:"viaAsync,omitempty"`
}

// Breakdown is a function's immediate callers and callees within one group,
// ranked by inclusive cost. It turns a hot function's inclusive time into "where
// it comes from" (callers) and "where it goes" (callees).
type Breakdown struct {
	Key     string          `json:"key"`
	Display string          `json:"display"`
	Package string          `json:"package,omitempty"`
	Callers []BreakdownEdge `json:"callers"`
	Callees []BreakdownEdge `json:"callees"`
	// Contexts is the distribution of logical owners (route/job labels) over this
	// function's inclusive time, when the profiles carried async-context data.
	// Empty otherwise. This answers "which route drives F" with real causality
	// rather than the stitched approximation in Callers.
	Contexts []BreakdownEdge `json:"contexts,omitempty"`
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
func BuildBreakdown(agg *v8profile.Aggregation, fnKey string, topN int, stitch bool) (Breakdown, bool) {
	fn := agg.Functions[fnKey]
	if fn == nil {
		return Breakdown{}, false
	}
	pc := profileCount(agg)
	bd := Breakdown{Key: fnKey, Display: fn.Display, Package: fn.Package}

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
	for label, m := range agg.FunctionContexts[fnKey] {
		bd.Contexts = append(bd.Contexts, edge(agg, label, m, pc))
	}

	bd.Callers = rankEdges(bd.Callers, topN)
	bd.Callees = rankEdges(bd.Callees, topN)
	bd.Contexts = rankEdges(bd.Contexts, topN)
	return bd, true
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
