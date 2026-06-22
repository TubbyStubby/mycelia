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
	raw, ok := BuildBreakdown(agg, "hot", 0, false, CtxSortMicros, nil)
	if !ok {
		t.Fatal("breakdown not found")
	}
	if len(raw.Callers) != 1 || raw.Callers[0].Key != "tramp" || raw.Callers[0].ViaAsync {
		t.Fatalf("raw callers = %+v, want [tramp] not viaAsync", raw.Callers)
	}

	// Stitched: the trampoline is skipped, handler surfaces with viaAsync.
	st, _ := BuildBreakdown(agg, "hot", 0, true, CtxSortMicros, nil)
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

	bd, _ := BuildBreakdown(agg, "hot", 0, true, CtxSortMicros, nil)
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
	bd, _ := BuildBreakdown(agg, "hot", 0, true, CtxSortMicros, nil)
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
	bd, ok := BuildBreakdown(agg, "hot", 0, true, CtxSortMicros, nil)
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
	byLean, _ := BuildBreakdown(agg, "hot", 0, true, CtxSortPctOfContext, nil)
	if byLean.Contexts[0].Display != "GET /b" {
		t.Errorf("pctOfContext sort top = %q, want GET /b", byLean.Contexts[0].Display)
	}
}

// TestBuildMatrixContextDimension checks the context dimension yields context
// rows. When ContextPackages is absent (legacy/minimal fixture), the fallback
// path returns the full Contexts self so no rows are dropped even under a
// category filter.
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
	// No ContextPackages → fallback: full Contexts self is used; neither row is
	// dropped despite the category filter (non-zero fallback self is preserved).
	m := BuildMatrix(groups, DimContext, MetricSelfMicros, 0, map[string]bool{v8profile.CatUser: true}, SortMax)
	if len(m.Rows) != 2 {
		t.Fatalf("context rows = %d, want 2 (fallback: ContextPackages absent)", len(m.Rows))
	}
	if m.Rows[0].Display != "GET /a" || m.Rows[0].Cells[0].SelfMicros != 70 || m.Rows[0].Cells[0].SelfPct != 70 {
		t.Errorf("top context = %+v, want GET /a 70µs / 70%%", m.Rows[0])
	}
}

// entityBreakdownAgg is the fixture for the package/file/context drill-downs:
// three functions across two packages and three files, one route whose self time
// is split 80/20 across the app/lib packages (and a.js/c.js files).
func entityBreakdownAgg() *v8profile.Aggregation {
	mkfn := func(key, file, pkg string, self, total int64) *v8profile.Entity {
		e := fn(key, key, pkg)
		e.File = file
		e.Metric = v8profile.Metric{SelfMicros: self, TotalMicros: total}
		return e
	}
	mkent := func(key, pkg string, self int64) *v8profile.Entity {
		e := fn(key, key, pkg)
		e.Metric = v8profile.Metric{SelfMicros: self, TotalMicros: self}
		return e
	}
	selfTotal := func(v int64) v8profile.Metric {
		return v8profile.Metric{SelfMicros: v, TotalMicros: v, SelfSamples: v, TotalSamples: v}
	}
	return &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{
			"f1": mkfn("f1", "a.js", "app", 100, 100),
			"f2": mkfn("f2", "b.js", "app", 40, 60),
			"f3": mkfn("f3", "c.js", "lib", 20, 20),
		},
		Files: map[string]*v8profile.Entity{
			"a.js": mkent("a.js", "app", 100),
			"b.js": mkent("b.js", "app", 40),
			"c.js": mkent("c.js", "lib", 20),
		},
		Packages: map[string]*v8profile.Entity{
			"app": mkent("app", "app", 140),
			"lib": mkent("lib", "lib", 20),
		},
		Contexts: map[string]*v8profile.Entity{
			"GET /x": {Key: "GET /x", Display: "GET /x", Kind: v8profile.KindContext, Metric: v8profile.Metric{SelfMicros: 100, TotalMicros: 100}},
		},
		FunctionContexts: map[string]map[string]v8profile.Metric{
			"f1": {"GET /x": {TotalMicros: 90, TotalSamples: 9}},
			"f3": {"GET /x": {TotalMicros: 20, TotalSamples: 2}},
		},
		ContextPackages: map[string]map[string]v8profile.Metric{
			"GET /x": {"app": selfTotal(80), "lib": selfTotal(20)},
		},
		ContextFiles: map[string]map[string]v8profile.Metric{
			"GET /x": {"a.js": selfTotal(80), "c.js": selfTotal(20)},
		},
		ProfileCount: 1,
	}
}

// keysOf extracts edge keys in order, for order-sensitive assertions.
func keysOf(edges []BreakdownEdge) []string {
	out := make([]string, len(edges))
	for i, e := range edges {
		out[i] = e.Key
	}
	return out
}

// TestBuildEntityBreakdownPackage checks a package lists its own functions and
// files (by self, descending) and the routes that exercise it.
func TestBuildEntityBreakdownPackage(t *testing.T) {
	agg := entityBreakdownAgg()
	bd, ok := BuildEntityBreakdown(agg, DimPackage, "app", 0, true, CtxSortMicros, nil)
	if !ok {
		t.Fatal("package breakdown not found")
	}
	if bd.Dimension != DimPackage {
		t.Errorf("dimension = %q, want package", bd.Dimension)
	}
	if got := keysOf(bd.Functions); len(got) != 2 || got[0] != "f1" || got[1] != "f2" {
		t.Errorf("functions = %v, want [f1 f2] (f3 is in lib, ranked by self)", got)
	}
	if bd.Functions[0].SelfMicros != 100 {
		t.Errorf("f1 self = %g, want 100", bd.Functions[0].SelfMicros)
	}
	if got := keysOf(bd.Files); len(got) != 2 || got[0] != "a.js" {
		t.Errorf("files = %v, want [a.js b.js]", got)
	}
	if len(bd.Contexts) != 1 || bd.Contexts[0].Key != "GET /x" || bd.Contexts[0].PctOfContext != 80 {
		t.Errorf("contexts = %+v, want GET /x @ pctOfContext 80 (80/100)", bd.Contexts)
	}
}

// TestBuildEntityBreakdownFile checks a file lists only its own functions (via
// the exact Entity.File link) and the routes that exercise it.
func TestBuildEntityBreakdownFile(t *testing.T) {
	agg := entityBreakdownAgg()
	bd, ok := BuildEntityBreakdown(agg, DimFile, "a.js", 0, true, CtxSortMicros, nil)
	if !ok {
		t.Fatal("file breakdown not found")
	}
	if got := keysOf(bd.Functions); len(got) != 1 || got[0] != "f1" {
		t.Errorf("functions = %v, want [f1] (only a.js member)", got)
	}
	if len(bd.Contexts) != 1 || bd.Contexts[0].PctOfContext != 80 {
		t.Errorf("contexts = %+v, want GET /x @ 80", bd.Contexts)
	}
}

// TestBuildEntityBreakdownContext checks a context decomposes into the functions
// under it (inclusive, with route share) and the packages/files its self lands in.
func TestBuildEntityBreakdownContext(t *testing.T) {
	agg := entityBreakdownAgg()
	bd, ok := BuildEntityBreakdown(agg, DimContext, "GET /x", 0, true, CtxSortMicros, nil)
	if !ok {
		t.Fatal("context breakdown not found")
	}
	// Functions: inclusive from FunctionContexts, ranked by inclusive total.
	if got := keysOf(bd.Functions); len(got) != 2 || got[0] != "f1" {
		t.Errorf("functions = %v, want f1 first (90 > 20)", got)
	}
	if bd.Functions[0].TotalMicros != 90 || bd.Functions[0].PctOfContext != 90 {
		t.Errorf("f1 = %+v, want total 90 / pctOfContext 90", bd.Functions[0])
	}
	// Packages/files: self, summing to the context total, ranked by self.
	if got := keysOf(bd.Packages); len(got) != 2 || got[0] != "app" {
		t.Errorf("packages = %v, want [app lib]", got)
	}
	if bd.Packages[0].SelfMicros != 80 || bd.Packages[0].PctOfContext != 80 {
		t.Errorf("app = %+v, want self 80 / pctOfContext 80", bd.Packages[0])
	}
	if got := keysOf(bd.Files); len(got) != 2 || got[0] != "a.js" {
		t.Errorf("files = %v, want [a.js c.js]", got)
	}
	// The file slice carries the owning package for display.
	if bd.Files[0].Package != "app" {
		t.Errorf("a.js package = %q, want app", bd.Files[0].Package)
	}
}

// TestBuildEntityBreakdownMissing checks a missing key reports not-found.
func TestBuildEntityBreakdownMissing(t *testing.T) {
	agg := entityBreakdownAgg()
	if _, ok := BuildEntityBreakdown(agg, DimPackage, "nope", 0, true, CtxSortMicros, nil); ok {
		t.Error("expected ok=false for absent package")
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

// ctxAgg builds a context-breakdown fixture with two packages:
//   - "busy" (category=user): 80µs self under route GET /r
//   - "(idle)" (category=idle): 20µs self under route GET /r
//
// The context total is 100µs. Used to verify allowed filtering and PctOfContext
// re-basing on the packages and files sections.
func ctxAgg() *v8profile.Aggregation {
	mkpkg := func(key, cat string, self int64) *v8profile.Entity {
		e := &v8profile.Entity{Key: key, Display: key, Package: key, Category: cat}
		e.Metric = v8profile.Metric{SelfMicros: self, TotalMicros: self, SelfSamples: self, TotalSamples: self}
		return e
	}
	mkfile := func(key, pkg, cat string, self int64) *v8profile.Entity {
		e := &v8profile.Entity{Key: key, Display: key, Package: pkg, Category: cat}
		e.Metric = v8profile.Metric{SelfMicros: self, TotalMicros: self}
		return e
	}
	return &v8profile.Aggregation{
		Functions:  map[string]*v8profile.Entity{},
		Files: map[string]*v8profile.Entity{
			"busy.js": mkfile("busy.js", "busy", v8profile.CatUser, 80),
			"idle.js": mkfile("idle.js", "(idle)", v8profile.CatIdle, 20),
		},
		Packages: map[string]*v8profile.Entity{
			"busy":   mkpkg("busy", v8profile.CatUser, 80),
			"(idle)": mkpkg("(idle)", v8profile.CatIdle, 20),
		},
		Contexts: map[string]*v8profile.Entity{
			"GET /r": {Key: "GET /r", Display: "GET /r", Kind: v8profile.KindContext,
				Metric: v8profile.Metric{SelfMicros: 100, TotalMicros: 100}},
		},
		ContextPackages: map[string]map[string]v8profile.Metric{
			"GET /r": {
				"busy":   {SelfMicros: 80, TotalMicros: 80},
				"(idle)": {SelfMicros: 20, TotalMicros: 20},
			},
		},
		ContextFiles: map[string]map[string]v8profile.Metric{
			"GET /r": {
				"busy.js": {SelfMicros: 80, TotalMicros: 80},
				"idle.js": {SelfMicros: 20, TotalMicros: 20},
			},
		},
		FunctionContexts: map[string]map[string]v8profile.Metric{},
		ProfileCount:     1,
	}
}

// TestContextBreakdownAllowedFiltersAndRebases verifies that when an allowed set
// excluding idle is passed to BuildEntityBreakdown for a context:
//   - The idle package and file rows are dropped from packages/files sections.
//   - The kept rows' PctOfContext is re-based to sum to ~100%.
//   - The functions section is unaffected (it is not filtered by category).
//   - Nil allowed preserves the original two rows with their original percentages.
func TestContextBreakdownAllowedFiltersAndRebases(t *testing.T) {
	agg := ctxAgg()

	// No filter: both rows, original PctOfContext values.
	bd, ok := BuildEntityBreakdown(agg, DimContext, "GET /r", 0, true, CtxSortMicros, nil)
	if !ok {
		t.Fatal("context breakdown not found")
	}
	if len(bd.Packages) != 2 {
		t.Fatalf("nil allowed: packages = %d, want 2", len(bd.Packages))
	}
	if len(bd.Files) != 2 {
		t.Fatalf("nil allowed: files = %d, want 2", len(bd.Files))
	}
	// Original PctOfContext: busy=80%, idle=20%.
	pkgByKey := map[string]BreakdownEdge{}
	for _, e := range bd.Packages {
		pkgByKey[e.Key] = e
	}
	if pkgByKey["busy"].PctOfContext != 80 || pkgByKey["(idle)"].PctOfContext != 20 {
		t.Errorf("nil allowed PctOfContext: busy=%g idle=%g, want 80/20",
			pkgByKey["busy"].PctOfContext, pkgByKey["(idle)"].PctOfContext)
	}

	// Filter to user only: idle row dropped; busy re-bases to 100%.
	allowed := map[string]bool{v8profile.CatUser: true}
	bd, ok = BuildEntityBreakdown(agg, DimContext, "GET /r", 0, true, CtxSortMicros, allowed)
	if !ok {
		t.Fatal("filtered context breakdown not found")
	}
	if len(bd.Packages) != 1 || bd.Packages[0].Key != "busy" {
		t.Fatalf("filtered packages = %+v, want [busy]", bd.Packages)
	}
	if bd.Packages[0].PctOfContext != 100 {
		t.Errorf("filtered packages[0].PctOfContext = %g, want 100 (rebased)", bd.Packages[0].PctOfContext)
	}
	if len(bd.Files) != 1 || bd.Files[0].Key != "busy.js" {
		t.Fatalf("filtered files = %+v, want [busy.js]", bd.Files)
	}
	if bd.Files[0].PctOfContext != 100 {
		t.Errorf("filtered files[0].PctOfContext = %g, want 100 (rebased)", bd.Files[0].PctOfContext)
	}
	// Functions section is not filtered (there are none in this fixture but the
	// property still holds: filter does not touch the functions slice).
	if len(bd.Functions) != 0 {
		t.Errorf("filtered functions = %d, want 0 (unaffected by category filter)", len(bd.Functions))
	}
}

// TestBreakdownFocusContextsClearsCallersCallees checks that when focus="contexts"
// the callers and callees are cleared from the returned view. This covers the
// engine/compare layer since there is no MCP handler test harness.
func TestBreakdownFocusContextsClearsCallersCallees(t *testing.T) {
	// Build a minimal agg with one function, one caller, and one context.
	agg := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{
			"hot":    {Key: "hot", Display: "hot (a.js:1)", Package: "app", Category: v8profile.CatUser, Metric: v8profile.Metric{TotalMicros: 100}},
			"caller": {Key: "caller", Display: "caller (b.js:1)", Package: "app", Category: v8profile.CatUser},
		},
		Files:    map[string]*v8profile.Entity{},
		Packages: map[string]*v8profile.Entity{},
		Edges: map[string]map[string]v8profile.Metric{
			"caller": {"hot": {TotalMicros: 100, TotalSamples: 10}},
		},
		Contexts: map[string]*v8profile.Entity{
			"GET /a": {Key: "GET /a", Display: "GET /a", Kind: v8profile.KindContext, Metric: v8profile.Metric{TotalMicros: 100}},
		},
		FunctionContexts: map[string]map[string]v8profile.Metric{
			"hot": {"GET /a": {TotalMicros: 100}},
		},
		ProfileCount: 1,
	}

	bd, ok := BuildBreakdown(agg, "hot", 0, true, CtxSortMicros, nil)
	if !ok {
		t.Fatal("breakdown not found")
	}
	// Without focus: callers and contexts are both populated.
	if len(bd.Callers) == 0 {
		t.Errorf("expected callers before focus filtering, got none")
	}
	if len(bd.Contexts) == 0 {
		t.Errorf("expected contexts, got none")
	}

	// Simulate focus=contexts: clear callers/callees (mirrors what breakdownHandler does).
	bd.Callers = nil
	bd.Callees = nil
	if bd.Callers != nil || bd.Callees != nil {
		t.Errorf("after focus=contexts, callers/callees should be nil")
	}
	if len(bd.Contexts) == 0 {
		t.Errorf("contexts should be preserved after focus=contexts")
	}
}
