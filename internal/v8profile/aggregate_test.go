package v8profile

import "testing"

// buildProfile is a small helper to construct a profile from nodes + a sample
// sequence with uniform 1µs deltas.
func buildProfile(nodes []Node, samples []int) *Profile {
	deltas := make([]int64, len(samples))
	for i := range deltas {
		deltas[i] = 1
	}
	return &Profile{
		Nodes:      nodes,
		Samples:    samples,
		TimeDeltas: deltas,
		StartTime:  0,
		EndTime:    int64(len(samples)),
	}
}

// TestAggregateSelfAndTotal checks self/total over a simple a -> b -> c chain.
func TestAggregateSelfAndTotal(t *testing.T) {
	// root(1) -> a(2) -> b(3) -> c(4). Samples land on b twice, c once, a once.
	nodes := []Node{
		{ID: 1, CallFrame: CallFrame{FunctionName: "(root)"}, Children: []int{2}},
		{ID: 2, CallFrame: CallFrame{FunctionName: "a", ScriptID: "1", URL: "file:///app/a.js", LineNumber: 0}, Children: []int{3}},
		{ID: 3, CallFrame: CallFrame{FunctionName: "b", ScriptID: "1", URL: "file:///app/a.js", LineNumber: 9}, Children: []int{4}},
		{ID: 4, CallFrame: CallFrame{FunctionName: "c", ScriptID: "2", URL: "file:///app/c.js", LineNumber: 0}},
	}
	// Samples: b twice, c once; a is never the leaf.
	p := buildProfile(nodes, []int{3, 3, 4})

	agg := AggregateProfile(p)

	if agg.Overall.SelfMicros != 3 {
		t.Fatalf("overall self = %d, want 3", agg.Overall.SelfMicros)
	}

	bKey := "1:10:b"
	b := agg.Functions[bKey]
	if b == nil {
		t.Fatalf("function %q missing; have %v", bKey, keys(agg.Functions))
	}
	if b.Metric.SelfMicros != 2 {
		t.Errorf("b self = %d, want 2", b.Metric.SelfMicros)
	}
	// b's inclusive total includes c (1) plus b's own self (2) = 3.
	if b.Metric.TotalMicros != 3 {
		t.Errorf("b total = %d, want 3", b.Metric.TotalMicros)
	}

	aKey := "1:1:a"
	a := agg.Functions[aKey]
	if a.Metric.SelfMicros != 0 {
		t.Errorf("a self = %d, want 0", a.Metric.SelfMicros)
	}
	if a.Metric.TotalMicros != 3 {
		t.Errorf("a total = %d, want 3 (all descendants)", a.Metric.TotalMicros)
	}
}

// TestAggregateRecursionNoDoubleCount verifies that a self-recursive function's
// inclusive total is not inflated by recursion.
func TestAggregateRecursionNoDoubleCount(t *testing.T) {
	// root(1) -> rec(2) -> rec(3) -> leaf(4); rec appears twice on the path.
	cf := CallFrame{FunctionName: "rec", ScriptID: "1", URL: "file:///app/r.js", LineNumber: 0}
	nodes := []Node{
		{ID: 1, CallFrame: CallFrame{FunctionName: "(root)"}, Children: []int{2}},
		{ID: 2, CallFrame: cf, Children: []int{3}},
		{ID: 3, CallFrame: cf, Children: []int{4}},
		{ID: 4, CallFrame: CallFrame{FunctionName: "leaf", ScriptID: "2", URL: "file:///app/l.js"}},
	}
	// 1 sample each on the two rec nodes and the leaf.
	p := buildProfile(nodes, []int{2, 3, 4})

	agg := AggregateProfile(p)
	rec := agg.Functions["1:1:rec"]
	if rec == nil {
		t.Fatalf("rec missing")
	}
	// Self = 2 (both rec nodes sampled once). Total must be 3 (rec self 2 + leaf
	// 1), NOT 5 from double-counting the nested rec subtree.
	if rec.Metric.SelfMicros != 2 {
		t.Errorf("rec self = %d, want 2", rec.Metric.SelfMicros)
	}
	if rec.Metric.TotalMicros != 3 {
		t.Errorf("rec total = %d, want 3 (recursion collapsed)", rec.Metric.TotalMicros)
	}
}

func TestMergeAggregations(t *testing.T) {
	nodes := []Node{
		{ID: 1, CallFrame: CallFrame{FunctionName: "(root)"}, Children: []int{2}},
		{ID: 2, CallFrame: CallFrame{FunctionName: "a", ScriptID: "1", URL: "file:///app/a.js"}},
	}
	a1 := AggregateProfile(buildProfile(nodes, []int{2, 2}))
	a2 := AggregateProfile(buildProfile(nodes, []int{2}))

	merged := MergeAggregations(a1, a2)
	if merged.ProfileCount != 2 {
		t.Errorf("profileCount = %d, want 2", merged.ProfileCount)
	}
	a := merged.Functions["1:1:a"]
	if a.Metric.SelfMicros != 3 {
		t.Errorf("merged a self = %d, want 3", a.Metric.SelfMicros)
	}
	if merged.Overall.SelfMicros != 3 {
		t.Errorf("merged overall = %d, want 3", merged.Overall.SelfMicros)
	}
}

// TestAggregateEdges checks that the call graph records each parent→child edge
// weighted by the child's subtree inclusive cost.
func TestAggregateEdges(t *testing.T) {
	// root(1) -> a(2) -> b(3) -> c(4). a calls b; b calls c.
	nodes := []Node{
		{ID: 1, CallFrame: CallFrame{FunctionName: "(root)"}, Children: []int{2}},
		{ID: 2, CallFrame: CallFrame{FunctionName: "a", ScriptID: "1", URL: "file:///app/a.js", LineNumber: 0}, Children: []int{3}},
		{ID: 3, CallFrame: CallFrame{FunctionName: "b", ScriptID: "1", URL: "file:///app/a.js", LineNumber: 9}, Children: []int{4}},
		{ID: 4, CallFrame: CallFrame{FunctionName: "c", ScriptID: "2", URL: "file:///app/c.js", LineNumber: 0}},
	}
	// b sampled twice, c once.
	agg := AggregateProfile(buildProfile(nodes, []int{3, 3, 4}))

	aKey, bKey, cKey := "1:1:a", "1:10:b", "2:1:c"
	// a -> b: b's subtree is b(2) + c(1) = 3.
	if got := agg.Edges[aKey][bKey].TotalMicros; got != 3 {
		t.Errorf("edge a->b = %d, want 3", got)
	}
	// b -> c: c's subtree is 1.
	if got := agg.Edges[bKey][cKey].TotalMicros; got != 1 {
		t.Errorf("edge b->c = %d, want 1", got)
	}
}

// TestBuildBreakdownAndMergeEdges checks caller/callee assembly off merged edges.
func TestBuildBreakdownAndMergeEdges(t *testing.T) {
	nodes := []Node{
		{ID: 1, CallFrame: CallFrame{FunctionName: "(root)"}, Children: []int{2}},
		{ID: 2, CallFrame: CallFrame{FunctionName: "a", ScriptID: "1", URL: "file:///app/a.js", LineNumber: 0}, Children: []int{3}},
		{ID: 3, CallFrame: CallFrame{FunctionName: "b", ScriptID: "1", URL: "file:///app/a.js", LineNumber: 9}, Children: []int{4}},
		{ID: 4, CallFrame: CallFrame{FunctionName: "c", ScriptID: "2", URL: "file:///app/c.js", LineNumber: 0}},
	}
	a1 := AggregateProfile(buildProfile(nodes, []int{3, 3, 4}))
	a2 := AggregateProfile(buildProfile(nodes, []int{3, 4}))
	merged := MergeAggregations(a1, a2)

	// a->b edge: profile1 b-subtree=3, profile2 b-subtree=2 (b once + c once) => 5.
	if got := merged.Edges["1:1:a"]["1:10:b"].TotalMicros; got != 5 {
		t.Fatalf("merged edge a->b = %d, want 5", got)
	}
}

// TestAggregateContexts checks per-context self and the inclusive
// FunctionContexts rollup (which logical owner drives each function).
func TestAggregateContexts(t *testing.T) {
	// root(1) -> handler(2) -> hydrate(3)
	nodes := []Node{
		{ID: 1, CallFrame: CallFrame{FunctionName: "(root)"}, Children: []int{2}},
		{ID: 2, CallFrame: CallFrame{FunctionName: "handler", ScriptID: "1", URL: "file:///app/h.js", LineNumber: 0}, Children: []int{3}},
		{ID: 3, CallFrame: CallFrame{FunctionName: "hydrate", ScriptID: "1", URL: "file:///app/h.js", LineNumber: 9}},
	}
	p := buildProfile(nodes, []int{3, 3, 2, 3}) // hydrate, hydrate, handler, hydrate
	// sample 0,2 -> route A; sample 1,3 -> route B
	p.Async = &AsyncContext{Version: 1, Labels: []string{"A", "B"}, Samples: []int{0, 1, 0, 1}}

	agg := AggregateProfile(p)

	if agg.Contexts["A"].Metric.SelfMicros != 2 || agg.Contexts["B"].Metric.SelfMicros != 2 {
		t.Errorf("context self = A:%d B:%d, want 2/2", agg.Contexts["A"].Metric.SelfMicros, agg.Contexts["B"].Metric.SelfMicros)
	}
	// handler inclusive by context: A = its self(1) + hydrate-under-A(1) = 2; B = 2.
	h := agg.FunctionContexts["1:1:handler"]
	if h["A"].TotalMicros != 2 || h["B"].TotalMicros != 2 {
		t.Errorf("handler contexts = A:%d B:%d, want 2/2", h["A"].TotalMicros, h["B"].TotalMicros)
	}
	// hydrate inclusive by context: A=1 (sample0), B=2 (samples1,3).
	hy := agg.FunctionContexts["1:10:hydrate"]
	if hy["A"].TotalMicros != 1 || hy["B"].TotalMicros != 2 {
		t.Errorf("hydrate contexts = A:%d B:%d, want 1/2", hy["A"].TotalMicros, hy["B"].TotalMicros)
	}
}

// TestAggregateContextRecursion checks the inclusive context rollup collapses
// recursion (a function's context total is not inflated when it recurs).
func TestAggregateContextRecursion(t *testing.T) {
	cf := CallFrame{FunctionName: "rec", ScriptID: "1", URL: "file:///app/r.js", LineNumber: 0}
	nodes := []Node{
		{ID: 1, CallFrame: CallFrame{FunctionName: "(root)"}, Children: []int{2}},
		{ID: 2, CallFrame: cf, Children: []int{3}},
		{ID: 3, CallFrame: cf, Children: []int{4}},
		{ID: 4, CallFrame: CallFrame{FunctionName: "leaf", ScriptID: "2", URL: "file:///app/l.js"}},
	}
	p := buildProfile(nodes, []int{2, 3, 4}) // both rec nodes + leaf, all one route
	p.Async = &AsyncContext{Version: 1, Labels: []string{"A"}, Samples: []int{0, 0, 0}}

	agg := AggregateProfile(p)
	// rec inclusive under A must be 3 (rec self 2 + leaf 1), not 5 from double count.
	if got := agg.FunctionContexts["1:1:rec"]["A"].TotalMicros; got != 3 {
		t.Errorf("rec context A total = %d, want 3 (recursion collapsed)", got)
	}
}

// TestAggregateNoContext confirms profiles without an _async block stay nil.
func TestAggregateNoContext(t *testing.T) {
	nodes := []Node{{ID: 1, CallFrame: CallFrame{FunctionName: "a", ScriptID: "1", URL: "file:///a.js"}}}
	agg := AggregateProfile(buildProfile(nodes, []int{1}))
	if agg.Contexts != nil || agg.FunctionContexts != nil {
		t.Errorf("expected nil context maps without _async, got %v / %v", agg.Contexts, agg.FunctionContexts)
	}
}

func keys(m map[string]*Entity) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
