package v8profile

import (
	"strings"
	"testing"
)

// blockingFixture builds a profile with two long-task episodes:
//   - an 80ms single-sample stall on workA (a long synchronous native/JS call
//     the sampler couldn't interrupt), under route "GET /a";
//   - a 60ms dense run of 60×1ms samples on workB, under route "GET /b".
//
// Each episode is bracketed by idle samples (the episode boundary).
func blockingFixture() *Profile {
	nodes := []Node{
		{ID: 1, CallFrame: CallFrame{FunctionName: "(root)"}, Children: []int{2, 4, 5}},
		{ID: 2, CallFrame: CallFrame{FunctionName: "handlerA", ScriptID: "ha", URL: "file:///app/a.js", LineNumber: 0}, Children: []int{3}},
		{ID: 3, CallFrame: CallFrame{FunctionName: "workA", ScriptID: "wa", URL: "file:///app/a.js", LineNumber: 9}},
		{ID: 4, CallFrame: CallFrame{FunctionName: "(idle)"}},
		{ID: 5, CallFrame: CallFrame{FunctionName: "handlerB", ScriptID: "hb", URL: "file:///app/b.js", LineNumber: 0}, Children: []int{6}},
		{ID: 6, CallFrame: CallFrame{FunctionName: "workB", ScriptID: "wb", URL: "file:///app/b.js", LineNumber: 4}},
	}

	samples := []int{4, 3, 4}
	deltas := []int64{1000, 80000, 1000}
	async := []int{-1, 0, -1}
	for i := 0; i < 60; i++ {
		samples = append(samples, 6)
		deltas = append(deltas, 1000)
		async = append(async, 1)
	}
	samples = append(samples, 4)
	deltas = append(deltas, 1000)
	async = append(async, -1)

	return &Profile{
		Nodes:      nodes,
		Samples:    samples,
		TimeDeltas: deltas,
		StartTime:  0,
		EndTime:    144000,
		Async:      &AsyncContext{Version: 1, Labels: []string{"GET /a", "GET /b"}, Samples: async},
	}
}

func TestDetectBlocking(t *testing.T) {
	agg := AggregateProfileWithThreshold(blockingFixture(), 50_000)
	b := agg.Blocking
	if b == nil {
		t.Fatal("Blocking is nil; want two episodes")
	}
	if b.Episodes != 2 {
		t.Errorf("episodes = %d, want 2", b.Episodes)
	}
	if b.BlockedMicros != 140_000 {
		t.Errorf("blockedMicros = %d, want 140000", b.BlockedMicros)
	}
	if b.MaxEpisodeMicros != 80_000 {
		t.Errorf("maxEpisode = %d, want 80000", b.MaxEpisodeMicros)
	}

	// Per-function attribution (keys: scriptId:line(1-based):name).
	if fa := b.Functions["wa:10:workA"]; fa == nil {
		t.Errorf("workA missing; have %v", blockKeys(b.Functions))
	} else if fa.BlockedMicros != 80_000 || fa.Episodes != 1 || fa.MaxEpisodeMicros != 80_000 {
		t.Errorf("workA = %+v, want blocked=80000 episodes=1 max=80000", *fa)
	}
	if fb := b.Functions["wb:5:workB"]; fb == nil {
		t.Errorf("workB missing; have %v", blockKeys(b.Functions))
	} else if fb.BlockedMicros != 60_000 || fb.Episodes != 1 {
		t.Errorf("workB = %+v, want blocked=60000 episodes=1", *fb)
	}

	// Per-context (route/API) attribution.
	if ca := b.Contexts["GET /a"]; ca == nil || ca.BlockedMicros != 80_000 || ca.Episodes != 1 {
		t.Errorf("context GET /a = %+v, want blocked=80000 episodes=1", ca)
	}
	if cb := b.Contexts["GET /b"]; cb == nil || cb.BlockedMicros != 60_000 || cb.Episodes != 1 {
		t.Errorf("context GET /b = %+v, want blocked=60000 episodes=1", cb)
	}

	// Top stalls: sorted by duration desc, with leaf + stack + context.
	if len(b.TopStalls) != 2 {
		t.Fatalf("topStalls = %d, want 2", len(b.TopStalls))
	}
	s0 := b.TopStalls[0]
	if s0.DurationMicros != 80_000 {
		t.Errorf("worst stall duration = %d, want 80000", s0.DurationMicros)
	}
	if s0.Context != "GET /a" {
		t.Errorf("worst stall context = %q, want GET /a", s0.Context)
	}
	if !strings.HasPrefix(s0.LeafDisplay, "workA") {
		t.Errorf("worst stall leaf = %q, want workA*", s0.LeafDisplay)
	}
	// Stack is root->leaf.
	if n := len(s0.Stack); n == 0 || !strings.HasPrefix(s0.Stack[0], "(root)") || !strings.HasPrefix(s0.Stack[n-1], "workA") {
		t.Errorf("worst stall stack = %v, want (root)…workA", s0.Stack)
	}
	if b.TopStalls[1].DurationMicros != 60_000 {
		t.Errorf("second stall duration = %d, want 60000", b.TopStalls[1].DurationMicros)
	}
}

// TestDetectBlockingThreshold checks that a higher threshold excludes both
// episodes (60ms and 80ms both below 200ms), yielding no Blocking data.
func TestDetectBlockingThreshold(t *testing.T) {
	agg := AggregateProfileWithThreshold(blockingFixture(), 200_000)
	if agg.Blocking != nil {
		t.Errorf("Blocking = %+v, want nil at a 200ms threshold", agg.Blocking)
	}
}

// TestMergeBlocking checks that merging two identical aggregations sums episode
// stats and keeps all stalls in descending order.
func TestMergeBlocking(t *testing.T) {
	a := AggregateProfileWithThreshold(blockingFixture(), 50_000)
	merged := MergeAggregations(a, AggregateProfileWithThreshold(blockingFixture(), 50_000))
	b := merged.Blocking
	if b == nil {
		t.Fatal("merged Blocking is nil")
	}
	if b.Episodes != 4 {
		t.Errorf("merged episodes = %d, want 4", b.Episodes)
	}
	if b.BlockedMicros != 280_000 {
		t.Errorf("merged blockedMicros = %d, want 280000", b.BlockedMicros)
	}
	if fa := b.Functions["wa:10:workA"]; fa == nil || fa.BlockedMicros != 160_000 || fa.Episodes != 2 {
		t.Errorf("merged workA = %+v, want blocked=160000 episodes=2", fa)
	}
	if len(b.TopStalls) != 4 {
		t.Fatalf("merged topStalls = %d, want 4", len(b.TopStalls))
	}
	for i := 1; i < len(b.TopStalls); i++ {
		if b.TopStalls[i-1].DurationMicros < b.TopStalls[i].DurationMicros {
			t.Errorf("topStalls not sorted desc at %d: %d < %d", i, b.TopStalls[i-1].DurationMicros, b.TopStalls[i].DurationMicros)
		}
	}
}

func blockKeys(m map[string]*BlockStat) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
