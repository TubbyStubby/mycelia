package compare

import (
	"testing"

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
	raw, ok := BuildBreakdown(agg, "hot", 0, false)
	if !ok {
		t.Fatal("breakdown not found")
	}
	if len(raw.Callers) != 1 || raw.Callers[0].Key != "tramp" || raw.Callers[0].ViaAsync {
		t.Fatalf("raw callers = %+v, want [tramp] not viaAsync", raw.Callers)
	}

	// Stitched: the trampoline is skipped, handler surfaces with viaAsync.
	st, _ := BuildBreakdown(agg, "hot", 0, true)
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

	bd, _ := BuildBreakdown(agg, "hot", 0, true)
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
	bd, _ := BuildBreakdown(agg, "hot", 0, true)
	if len(bd.Callers) != 1 || bd.Callers[0].Key != "tramp" || bd.Callers[0].TotalMicros != 50 {
		t.Errorf("callers = %+v, want trampoline kept @50 (cost not lost)", bd.Callers)
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
