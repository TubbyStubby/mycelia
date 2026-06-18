package compare

import (
	"testing"

	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/v8profile"
)

// aggWith builds a one-function aggregation with the given summed self-micros
// across profileCount merged profiles.
func aggWith(selfMicros int64, profileCount int) *v8profile.Aggregation {
	return &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{
			"k": {Key: "k", Display: "f", Metric: v8profile.Metric{SelfMicros: selfMicros, TotalMicros: selfMicros}},
		},
		Files:        map[string]*v8profile.Entity{},
		Packages:     map[string]*v8profile.Entity{},
		Overall:      v8profile.Metric{SelfMicros: selfMicros},
		ProfileCount: profileCount,
	}
}

// TestBuildMatrixAverages verifies that summed metrics are divided by the
// profile count so groups of different sizes compare on a per-profile basis.
func TestBuildMatrixAverages(t *testing.T) {
	groups := []GroupAggregation{
		// 100µs over 10 profiles => 10µs/profile.
		{ID: profiles.GroupID{Date: "2024-01-01", BuildTag: "a"}, Agg: aggWith(100, 10), TotalProfiles: 500},
		// 30µs over 3 profiles => 10µs/profile (same per-profile cost).
		{ID: profiles.GroupID{Date: "2024-01-02", BuildTag: "b"}, Agg: aggWith(30, 3), TotalProfiles: 3},
	}

	m := BuildMatrix(groups, DimFunction, MetricSelfMicros, 0, nil, SortMax)
	if len(m.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(m.Rows))
	}
	cells := m.Rows[0].Cells
	if cells[0].SelfMicros != 10 || cells[1].SelfMicros != 10 {
		t.Errorf("per-profile self = %v / %v, want 10 / 10", cells[0].SelfMicros, cells[1].SelfMicros)
	}

	if m.Summaries[0].TotalProfiles != 500 || m.Summaries[0].ProfileCount != 10 {
		t.Errorf("summary[0] profiles = %d of %d, want 10 of 500",
			m.Summaries[0].ProfileCount, m.Summaries[0].TotalProfiles)
	}
	if m.Summaries[0].OverallMicros != 10 {
		t.Errorf("summary[0] overall/profile = %v, want 10", m.Summaries[0].OverallMicros)
	}
}

// TestBuildMatrixCategoryFilter checks that disabled categories are excluded
// from rows and from the recomputed overall.
func TestBuildMatrixCategoryFilter(t *testing.T) {
	agg := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{
			"u": {Key: "u", Display: "app", Category: v8profile.CatUser, Metric: v8profile.Metric{SelfMicros: 6}},
			"i": {Key: "i", Display: "(idle)", Category: v8profile.CatIdle, Metric: v8profile.Metric{SelfMicros: 4}},
		},
		Packages: map[string]*v8profile.Entity{
			"app":    {Key: "app", Category: v8profile.CatUser, Metric: v8profile.Metric{SelfMicros: 6}},
			"(idle)": {Key: "(idle)", Category: v8profile.CatIdle, Metric: v8profile.Metric{SelfMicros: 4}},
		},
		Files:        map[string]*v8profile.Entity{},
		Overall:      v8profile.Metric{SelfMicros: 10},
		ProfileCount: 1,
	}
	groups := []GroupAggregation{{ID: profiles.GroupID{Date: "d", BuildTag: "b"}, Agg: agg, TotalProfiles: 1}}

	// Idle excluded.
	allowed := map[string]bool{v8profile.CatUser: true}
	m := BuildMatrix(groups, DimFunction, MetricSelfMicros, 0, allowed, SortMax)
	if len(m.Rows) != 1 || m.Rows[0].Display != "app" {
		t.Fatalf("rows = %+v, want only the user row", m.Rows)
	}
	// Overall headline should reflect only the user package (6), not 10.
	if m.Summaries[0].OverallMicros != 6 {
		t.Errorf("filtered overall = %v, want 6", m.Summaries[0].OverallMicros)
	}
}

// TestBuildMatrixBusyAndIdle checks the busy-normalized metric and the
// idle/busy summary split: both are computed against the unfiltered overall, so
// they describe composition independent of how busy the group was.
func TestBuildMatrixBusyAndIdle(t *testing.T) {
	agg := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{
			"m": {Key: "m", Display: "mongoose", Category: v8profile.CatNodeModules,
				Metric: v8profile.Metric{SelfMicros: 100, TotalMicros: 100}},
		},
		Packages: map[string]*v8profile.Entity{
			"mongoose": {Key: "mongoose", Category: v8profile.CatNodeModules, Metric: v8profile.Metric{SelfMicros: 100}},
			"app":      {Key: "app", Category: v8profile.CatUser, Metric: v8profile.Metric{SelfMicros: 300}},
			"(idle)":   {Key: "(idle)", Category: v8profile.CatIdle, Metric: v8profile.Metric{SelfMicros: 600}},
		},
		Files:        map[string]*v8profile.Entity{},
		Overall:      v8profile.Metric{SelfMicros: 1000},
		ProfileCount: 1,
	}
	groups := []GroupAggregation{{ID: profiles.GroupID{Date: "d", BuildTag: "b"}, Agg: agg, TotalProfiles: 1}}

	m := BuildMatrix(groups, DimFunction, MetricSelfMicros, 0, nil, SortMax)
	c := m.Rows[0].Cells[0]
	// selfPct against overall (1000); selfPctBusy against busy (1000-600=400).
	if c.SelfPct != 10 {
		t.Errorf("selfPct = %v, want 10", c.SelfPct)
	}
	if c.SelfPctBusy != 25 {
		t.Errorf("selfPctBusy = %v, want 25", c.SelfPctBusy)
	}
	if s := m.Summaries[0]; s.IdlePct != 60 || s.BusyPct != 40 {
		t.Errorf("idle/busy = %v/%v, want 60/40", s.IdlePct, s.BusyPct)
	}
}

// TestBuildMatrixDeltaSort checks delta/deltaPct ranking, the per-row Delta
// field, and that a newly-appeared entity (no baseline) ranks first by deltaPct.
func TestBuildMatrixDeltaSort(t *testing.T) {
	// Group 0 has a (100) and c (80); group 1 has a (110), c (70), and a newly
	// appeared b (50).
	g0 := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{
			"a": {Key: "a", Display: "a", Metric: v8profile.Metric{SelfMicros: 100}},
			"c": {Key: "c", Display: "c", Metric: v8profile.Metric{SelfMicros: 80}},
		},
		Files: map[string]*v8profile.Entity{}, Packages: map[string]*v8profile.Entity{},
		Overall: v8profile.Metric{SelfMicros: 1000}, ProfileCount: 1,
	}
	g1 := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{
			"a": {Key: "a", Display: "a", Metric: v8profile.Metric{SelfMicros: 110}},
			"b": {Key: "b", Display: "b", Metric: v8profile.Metric{SelfMicros: 50}},
			"c": {Key: "c", Display: "c", Metric: v8profile.Metric{SelfMicros: 70}},
		},
		Files: map[string]*v8profile.Entity{}, Packages: map[string]*v8profile.Entity{},
		Overall: v8profile.Metric{SelfMicros: 1000}, ProfileCount: 1,
	}
	groups := []GroupAggregation{
		{ID: profiles.GroupID{BuildTag: "g0"}, Agg: g0, TotalProfiles: 1},
		{ID: profiles.GroupID{BuildTag: "g1"}, Agg: g1, TotalProfiles: 1},
	}

	order := func(m Matrix) []string {
		got := make([]string, len(m.Rows))
		for i, r := range m.Rows {
			got[i] = r.Display
		}
		return got
	}

	// Max: a(110) > c(80) > b(50).
	if got := order(BuildMatrix(groups, DimFunction, MetricSelfMicros, 0, nil, SortMax)); !equal(got, []string{"a", "c", "b"}) {
		t.Errorf("sort max order = %v, want [a c b]", got)
	}
	// Delta: b(+50) > a(+10) > c(-10).
	mDelta := BuildMatrix(groups, DimFunction, MetricSelfMicros, 0, nil, SortDelta)
	if got := order(mDelta); !equal(got, []string{"b", "a", "c"}) {
		t.Errorf("sort delta order = %v, want [b a c]", got)
	}
	if mDelta.Rows[0].Delta != 50 {
		t.Errorf("b delta = %v, want 50", mDelta.Rows[0].Delta)
	}
	// DeltaPct: b (newly appeared) first, then a (+10%), then c (-12.5%).
	if got := order(BuildMatrix(groups, DimFunction, MetricSelfMicros, 0, nil, SortDeltaPct)); !equal(got, []string{"b", "a", "c"}) {
		t.Errorf("sort deltaPct order = %v, want [b a c]", got)
	}
	// Newly-appeared b has no baseline, so its DeltaPct field stays 0 (JSON-safe).
	for _, r := range mDelta.Rows {
		if r.Display == "b" && r.DeltaPct != 0 {
			t.Errorf("b deltaPct = %v, want 0 (no baseline)", r.DeltaPct)
		}
	}
}

// TestBuildBreakdown checks caller/callee resolution and per-profile averaging.
func TestBuildBreakdown(t *testing.T) {
	agg := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{
			"caller": {Key: "caller", Display: "caller fn", Package: "app"},
			"hot":    {Key: "hot", Display: "hot fn", Package: "app"},
			"callee": {Key: "callee", Display: "callee fn", Package: "mongoose"},
		},
		Files:    map[string]*v8profile.Entity{},
		Packages: map[string]*v8profile.Entity{},
		Edges: map[string]map[string]v8profile.Metric{
			"caller": {"hot": {TotalMicros: 200, TotalSamples: 20}},
			"hot":    {"callee": {TotalMicros: 80, TotalSamples: 8}},
		},
		Overall:      v8profile.Metric{SelfMicros: 1000},
		ProfileCount: 2, // values should be halved
	}

	bd, ok := BuildBreakdown(agg, "hot", 0, false)
	if !ok {
		t.Fatal("breakdown for hot not found")
	}
	if len(bd.Callers) != 1 || bd.Callers[0].Key != "caller" || bd.Callers[0].TotalMicros != 100 {
		t.Errorf("callers = %+v, want caller@100/profile", bd.Callers)
	}
	if len(bd.Callees) != 1 || bd.Callees[0].Display != "callee fn" || bd.Callees[0].TotalMicros != 40 {
		t.Errorf("callees = %+v, want callee@40/profile", bd.Callees)
	}

	if _, ok := BuildBreakdown(agg, "nonesuch", 0, false); ok {
		t.Error("expected ok=false for unknown function")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
