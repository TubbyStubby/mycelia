package compare

import (
	"testing"

	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/v8profile"
)

// fn is a tiny Entity constructor for breakdown tests.
func fn(key, display, pkg string) *v8profile.Entity {
	return &v8profile.Entity{Key: key, Display: display, Package: pkg}
}

// TestStitchThroughTrampoline checks that a transparent caller (runMicrotasks)
// is skipped and its cost attributed up to the real handler, marked viaAsync —
// while the non-stitched view still reports the trampoline as the caller.
func TestStitchThroughTrampoline(t *testing.T) {
	// handler -> runMicrotasks -> hot. runMicrotasks is transparent.
	agg := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{
			"hot":     fn("hot", "$__init (mongoose/document.js:10)", "mongoose"),
			"tramp":   fn("tramp", "runMicrotasks", ""),
			"handler": fn("handler", "routeHandler (app/routes.js:5)", "app"),
		},
		Files:    map[string]*v8profile.Entity{},
		Packages: map[string]*v8profile.Entity{},
		Edges: map[string]map[string]v8profile.Metric{
			"tramp":   {"hot": {TotalMicros: 100, TotalSamples: 10}},
			"handler": {"tramp": {TotalMicros: 100, TotalSamples: 10}},
		},
		ProfileCount: 1,
	}

	// Raw: the immediate caller is the trampoline.
	raw, ok := BuildBreakdown(agg, "hot", 0, false, CtxSortMicros)
	if !ok {
		t.Fatal("breakdown not found")
	}
	if len(raw.Callers) != 1 || raw.Callers[0].Key != "tramp" || raw.Callers[0].ViaAsync {
		t.Fatalf("raw callers = %+v, want [tramp] not viaAsync", raw.Callers)
	}

	// Stitched: the trampoline is skipped, handler surfaces with viaAsync.
	st, _ := BuildBreakdown(agg, "hot", 0, true, CtxSortMicros)
	if len(st.Callers) != 1 {
		t.Fatalf("stitched callers = %+v, want 1", st.Callers)
	}
	c := st.Callers[0]
	if c.Key != "handler" || !c.ViaAsync || c.TotalMicros != 100 {
		t.Errorf("stitched caller = %+v, want handler@100 viaAsync", c)
	}
}

// TestStitchProportionalSplit checks that when a transparent frame has multiple
// real callers, the hot function's cost is split proportionally to their inbound
// edge weights.
func TestStitchProportionalSplit(t *testing.T) {
	// a (75) and b (25) both call runMicrotasks; runMicrotasks calls hot (100).
	agg := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{
			"hot":   fn("hot", "hot (x.js:1)", "app"),
			"tramp": fn("tramp", "runMicrotasks", ""),
			"a":     fn("a", "a (x.js:2)", "app"),
			"b":     fn("b", "b (x.js:3)", "app"),
		},
		Files:    map[string]*v8profile.Entity{},
		Packages: map[string]*v8profile.Entity{},
		Edges: map[string]map[string]v8profile.Metric{
			"tramp": {"hot": {TotalMicros: 100, TotalSamples: 100}},
			"a":     {"tramp": {TotalMicros: 75}},
			"b":     {"tramp": {TotalMicros: 25}},
		},
		ProfileCount: 1,
	}

	bd, _ := BuildBreakdown(agg, "hot", 0, true, CtxSortMicros)
	got := map[string]float64{}
	for _, c := range bd.Callers {
		got[c.Key] = c.TotalMicros
		if !c.ViaAsync {
			t.Errorf("caller %s should be viaAsync", c.Key)
		}
	}
	if got["a"] != 75 || got["b"] != 25 {
		t.Errorf("split = %v, want a=75 b=25 (proportional)", got)
	}
}

// TestStitchTopTrampolineKept checks that a transparent frame with no callers of
// its own (a true top trampoline) is kept rather than dropping the cost.
func TestStitchTopTrampolineKept(t *testing.T) {
	agg := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{
			"hot":   fn("hot", "hot (x.js:1)", "app"),
			"tramp": fn("tramp", "(program)", ""),
		},
		Files:        map[string]*v8profile.Entity{},
		Packages:     map[string]*v8profile.Entity{},
		Edges:        map[string]map[string]v8profile.Metric{"tramp": {"hot": {TotalMicros: 50}}},
		ProfileCount: 1,
	}
	bd, _ := BuildBreakdown(agg, "hot", 0, true, CtxSortMicros)
	if len(bd.Callers) != 1 || bd.Callers[0].Key != "tramp" || bd.Callers[0].TotalMicros != 50 {
		t.Errorf("callers = %+v, want trampoline kept @50 (cost not lost)", bd.Callers)
	}
}

// TestBuildBreakdownContexts checks the per-context owner rollup is surfaced and
// per-profile averaged, and that pctOfFunction / pctOfContext are computed from
// the summed values (function inclusive total and each context's busy total).
func TestBuildBreakdownContexts(t *testing.T) {
	hot := fn("hot", "hot fn", "app")
	hot.Metric = v8profile.Metric{TotalMicros: 400} // function's own inclusive total
	agg := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{"hot": hot},
		Files:     map[string]*v8profile.Entity{},
		Packages:  map[string]*v8profile.Entity{},
		// /a owns 300µs of hot but is a small route (busy 600 → hot is 50% of it);
		// /b owns only 100µs of hot but is a tiny route (busy 200 → hot is 50%).
		Contexts: map[string]*v8profile.Entity{
			"GET /a": {Key: "GET /a", Display: "GET /a", Kind: v8profile.KindContext, Metric: v8profile.Metric{TotalMicros: 600}},
			"GET /b": {Key: "GET /b", Display: "GET /b", Kind: v8profile.KindContext, Metric: v8profile.Metric{TotalMicros: 125}},
		},
		FunctionContexts: map[string]map[string]v8profile.Metric{
			"hot": {"GET /a": {TotalMicros: 300}, "GET /b": {TotalMicros: 100}},
		},
		ProfileCount: 2, // halve micros, but ratios are averaging-invariant
	}
	bd, ok := BuildBreakdown(agg, "hot", 0, true, CtxSortMicros)
	if !ok {
		t.Fatal("not found")
	}
	if len(bd.Contexts) != 2 {
		t.Fatalf("contexts = %+v, want 2", bd.Contexts)
	}
	a := bd.Contexts[0]
	if a.Display != "GET /a" || a.TotalMicros != 150 {
		t.Errorf("ctx[0] = %+v, want GET /a @150 (300/2)", a)
	}
	// pctOfFunction: 300/400 = 75%. pctOfContext: 300/600 = 50%.
	if a.PctOfFunction != 75 || a.PctOfContext != 50 {
		t.Errorf("ctx[0] pcts = (ofFn %g, ofCtx %g), want (75, 50)", a.PctOfFunction, a.PctOfContext)
	}
	b := bd.Contexts[1]
	// /b: pctOfFunction 100/400 = 25%; pctOfContext 100/125 = 80%.
	if b.PctOfFunction != 25 || b.PctOfContext != 80 {
		t.Errorf("ctx[1] pcts = (ofFn %g, ofCtx %g), want (25, 80)", b.PctOfFunction, b.PctOfContext)
	}

	// Sorting by route share flips the order: /b (80%) outranks /a (50%) despite
	// owning a third of the absolute micros.
	byLean, _ := BuildBreakdown(agg, "hot", 0, true, CtxSortPctOfContext)
	if byLean.Contexts[0].Display != "GET /b" {
		t.Errorf("pctOfContext sort top = %q, want GET /b", byLean.Contexts[0].Display)
	}
}

// TestBuildMatrixContextDimension checks the context dimension yields context
// rows and ignores the category filter (a context spans categories).
func TestBuildMatrixContextDimension(t *testing.T) {
	agg := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{},
		Files:     map[string]*v8profile.Entity{},
		Packages:  map[string]*v8profile.Entity{},
		Contexts: map[string]*v8profile.Entity{
			"GET /a": {Key: "GET /a", Display: "GET /a", Kind: v8profile.KindContext, Metric: v8profile.Metric{SelfMicros: 70, TotalMicros: 70}},
			"GET /b": {Key: "GET /b", Display: "GET /b", Kind: v8profile.KindContext, Metric: v8profile.Metric{SelfMicros: 30, TotalMicros: 30}},
		},
		Overall:      v8profile.Metric{SelfMicros: 100},
		ProfileCount: 1,
	}
	groups := []GroupAggregation{{ID: profiles.GroupID{BuildTag: "b"}, Agg: agg, TotalProfiles: 1}}
	// A category filter must NOT drop context rows.
	m := BuildMatrix(groups, DimContext, MetricSelfMicros, 0, map[string]bool{v8profile.CatUser: true}, SortMax)
	if len(m.Rows) != 2 {
		t.Fatalf("context rows = %d, want 2 (category filter ignored)", len(m.Rows))
	}
	if m.Rows[0].Display != "GET /a" || m.Rows[0].Cells[0].SelfMicros != 70 || m.Rows[0].Cells[0].SelfPct != 70 {
		t.Errorf("top context = %+v, want GET /a 70µs / 70%%", m.Rows[0])
	}
}

func TestIsTransparentFrame(t *testing.T) {
	cases := []struct {
		e    *v8profile.Entity
		want bool
	}{
		{fn("", "runMicrotasks", ""), true},
		{fn("", "syncWrapper (kareem/index.js:328)", "kareem"), true},
		{fn("", "syncWrapper (app/util.js:1)", "app"), false}, // same name, wrong package
		{fn("", "routeHandler (app/routes.js:5)", "app"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isTransparentFrame(c.e); got != c.want {
			t.Errorf("isTransparentFrame(%v) = %v, want %v", c.e, got, c.want)
		}
	}
}
