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

	m := BuildMatrix(groups, DimFunction, MetricSelfMicros, 0, nil)
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
	m := BuildMatrix(groups, DimFunction, MetricSelfMicros, 0, allowed)
	if len(m.Rows) != 1 || m.Rows[0].Display != "app" {
		t.Fatalf("rows = %+v, want only the user row", m.Rows)
	}
	// Overall headline should reflect only the user package (6), not 10.
	if m.Summaries[0].OverallMicros != 6 {
		t.Errorf("filtered overall = %v, want 6", m.Summaries[0].OverallMicros)
	}
}
