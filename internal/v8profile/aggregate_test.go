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

func keys(m map[string]*Entity) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
