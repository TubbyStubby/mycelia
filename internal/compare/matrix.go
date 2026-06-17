// Package compare builds N-group side-by-side comparison matrices from per-group
// aggregations across the overall/package/function/file dimensions.
package compare

import (
	"sort"

	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/v8profile"
)

// Dimension selects the comparison granularity.
type Dimension string

const (
	DimOverall  Dimension = "overall"
	DimPackage  Dimension = "package"
	DimFunction Dimension = "function"
	DimFile     Dimension = "file"
)

// Metric selects which value ranks rows and drives the trend sparkline.
type Metric string

const (
	MetricSelfMicros   Metric = "selfMicros"
	MetricTotalMicros  Metric = "totalMicros"
	MetricSelfSamples  Metric = "selfSamples"
	MetricTotalSamples Metric = "totalSamples"
	MetricSelfPct      Metric = "selfPct"  // self micros as % of group total
	MetricTotalPct     Metric = "totalPct" // total micros as % of group total
)

// GroupAggregation pairs a group identity with its merged aggregation.
// TotalProfiles is the number of profiles in the group before sampling.
type GroupAggregation struct {
	ID            profiles.GroupID
	Agg           *v8profile.Aggregation
	TotalProfiles int
}

// Cell is one entity's metrics within one group. Sample/micros values are
// per-profile averages (summed metric divided by the number of profiles merged)
// so groups of different sizes — and sampled vs full groups — compare fairly.
// SelfPct/TotalPct are shares of the group's whole (unfiltered) total self time,
// so they stay stable regardless of the category filter.
type Cell struct {
	SelfSamples  float64 `json:"selfSamples"`
	TotalSamples float64 `json:"totalSamples"`
	SelfMicros   float64 `json:"selfMicros"`
	TotalMicros  float64 `json:"totalMicros"`
	SelfPct      float64 `json:"selfPct"`  // self micros as % of the group's overall self
	TotalPct     float64 `json:"totalPct"` // total micros as % of the group's overall self
}

// Row is one entity (function/file/package) across all groups.
type Row struct {
	Key     string    `json:"key"`
	Display string    `json:"display"`
	Package string    `json:"package"`
	Cells   []Cell    `json:"cells"` // aligned with Matrix.Groups
	Trend   []float64 `json:"trend"` // the selected metric per group, for sparklines
}

// GroupSummary holds per-group headline numbers shown above the table. Micros
// and sample figures are per-profile averages. ProfileCount is the number of
// profiles actually merged (after sampling); TotalProfiles is the group size
// before sampling.
type GroupSummary struct {
	ID                profiles.GroupID `json:"id"`
	OverallMicros     float64          `json:"overallMicros"`
	OverallSamples    float64          `json:"overallSamples"`
	SampleCount       int              `json:"sampleCount"`
	ProfileCount      int              `json:"profileCount"`
	TotalProfiles     int              `json:"totalProfiles"`
	DurationMicros    float64          `json:"durationMicros"`
	TimingApproximate bool             `json:"timingApproximate"`
}

// Matrix is the full comparison payload for one dimension.
type Matrix struct {
	Dimension Dimension          `json:"dimension"`
	Metric    Metric             `json:"metric"`
	Groups    []profiles.GroupID `json:"groups"`
	Summaries []GroupSummary     `json:"summaries"`
	Rows      []Row              `json:"rows"`
}

// BuildMatrix assembles a comparison matrix. groups are used as ordered columns;
// rows are the union of entities across groups for the given dimension, ranked
// by the selected metric (descending), optionally capped at topN (0 = no cap).
// allowed restricts rows to the given filter categories (native|node_modules|
// user|idle); a nil allowed set includes everything.
func BuildMatrix(groups []GroupAggregation, dim Dimension, metric Metric, topN int, allowed map[string]bool) Matrix {
	if metric == "" {
		metric = MetricSelfMicros
	}
	m := Matrix{
		Dimension: dim,
		Metric:    metric,
		Groups:    make([]profiles.GroupID, len(groups)),
		Summaries: make([]GroupSummary, len(groups)),
	}
	for i, g := range groups {
		m.Groups[i] = g.ID
		m.Summaries[i] = summarize(g, allowed)
	}

	if dim == DimOverall {
		m.Rows = overallRows(groups, allowed)
		return m
	}

	// Collect the union of entity keys across all groups.
	type rowAcc struct {
		display string
		pkg     string
		cells   []Cell
	}
	rows := map[string]*rowAcc{}
	ensure := func(key string) *rowAcc {
		r := rows[key]
		if r == nil {
			r = &rowAcc{cells: make([]Cell, len(groups))}
			rows[key] = r
		}
		return r
	}

	for gi, g := range groups {
		entities := entityMap(g.Agg, dim)
		overallSelf := g.Agg.Overall.SelfMicros
		pc := profileCount(g.Agg)
		for key, e := range entities {
			if allowed != nil && !allowed[e.Category] {
				continue
			}
			r := ensure(key)
			if r.display == "" {
				r.display = e.Display
				r.pkg = e.Package
			}
			selfPct, totalPct := 0.0, 0.0
			if overallSelf > 0 {
				// Ratio is unaffected by averaging; compute from summed values.
				selfPct = float64(e.Metric.SelfMicros) / float64(overallSelf) * 100
				totalPct = float64(e.Metric.TotalMicros) / float64(overallSelf) * 100
			}
			r.cells[gi] = Cell{
				SelfSamples:  avg(e.Metric.SelfSamples, pc),
				TotalSamples: avg(e.Metric.TotalSamples, pc),
				SelfMicros:   avg(e.Metric.SelfMicros, pc),
				TotalMicros:  avg(e.Metric.TotalMicros, pc),
				SelfPct:      selfPct,
				TotalPct:     totalPct,
			}
		}
	}

	out := make([]Row, 0, len(rows))
	for key, r := range rows {
		trend := make([]float64, len(r.cells))
		for i, c := range r.cells {
			trend[i] = cellMetric(c, metric)
		}
		out = append(out, Row{
			Key:     key,
			Display: r.display,
			Package: r.pkg,
			Cells:   r.cells,
			Trend:   trend,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		mi, mj := rowMax(out[i], metric), rowMax(out[j], metric)
		if mi != mj {
			return mi > mj
		}
		return out[i].Display < out[j].Display
	})

	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	m.Rows = out
	return m
}

func summarize(g GroupAggregation, allowed map[string]bool) GroupSummary {
	pc := profileCount(g.Agg)
	// Overall headline reflects only the enabled categories (packages partition
	// the profile, so summing allowed packages gives the filtered total).
	overall := filteredOverall(g.Agg, allowed)
	return GroupSummary{
		ID:                g.ID,
		OverallMicros:     avg(overall.SelfMicros, pc),
		OverallSamples:    avg(overall.SelfSamples, pc),
		SampleCount:       g.Agg.SampleCount,
		ProfileCount:      g.Agg.ProfileCount,
		TotalProfiles:     g.TotalProfiles,
		DurationMicros:    avg(g.Agg.DurationMicros, pc),
		TimingApproximate: g.Agg.TimingApproximate,
	}
}

// overallRows produces a single synthetic "total" row so the Overall tab can
// chart per-profile group-level totals, honoring the category filter.
func overallRows(groups []GroupAggregation, allowed map[string]bool) []Row {
	cells := make([]Cell, len(groups))
	trend := make([]float64, len(groups))
	for i, g := range groups {
		pc := profileCount(g.Agg)
		o := filteredOverall(g.Agg, allowed)
		cells[i] = Cell{
			SelfSamples:  avg(o.SelfSamples, pc),
			TotalSamples: avg(o.TotalSamples, pc),
			SelfMicros:   avg(o.SelfMicros, pc),
			TotalMicros:  avg(o.TotalMicros, pc),
			SelfPct:      100,
			TotalPct:     100,
		}
		trend[i] = cells[i].SelfMicros
	}
	return []Row{{
		Key:     "__overall__",
		Display: "Total (all frames, per profile)",
		Cells:   cells,
		Trend:   trend,
	}}
}

// filteredOverall sums the self/total of packages in the allowed categories.
// Packages partition the profile (every frame belongs to exactly one package),
// so this is the group's total restricted to the enabled categories. A nil
// allowed set returns the unfiltered overall.
func filteredOverall(a *v8profile.Aggregation, allowed map[string]bool) v8profile.Metric {
	if allowed == nil {
		return a.Overall
	}
	var m v8profile.Metric
	for _, e := range a.Packages {
		if allowed[e.Category] {
			m.SelfSamples += e.Metric.SelfSamples
			m.SelfMicros += e.Metric.SelfMicros
		}
	}
	// Total mirrors self at the overall level.
	m.TotalSamples = m.SelfSamples
	m.TotalMicros = m.SelfMicros
	return m
}

// profileCount returns the number of profiles merged into an aggregation,
// clamped to at least 1 to avoid division by zero.
func profileCount(a *v8profile.Aggregation) int {
	if a.ProfileCount < 1 {
		return 1
	}
	return a.ProfileCount
}

// avg divides a summed metric by the profile count to yield a per-profile mean.
func avg(sum int64, profileCount int) float64 {
	return float64(sum) / float64(profileCount)
}

func entityMap(a *v8profile.Aggregation, dim Dimension) map[string]*v8profile.Entity {
	switch dim {
	case DimPackage:
		return a.Packages
	case DimFile:
		return a.Files
	default:
		return a.Functions
	}
}

func cellMetric(c Cell, metric Metric) float64 {
	switch metric {
	case MetricTotalMicros:
		return c.TotalMicros
	case MetricSelfSamples:
		return c.SelfSamples
	case MetricTotalSamples:
		return c.TotalSamples
	case MetricSelfPct:
		return c.SelfPct
	case MetricTotalPct:
		return c.TotalPct
	default:
		return c.SelfMicros
	}
}

// rowMax returns the largest value of the selected metric across a row's cells,
// used for ranking rows.
func rowMax(r Row, metric Metric) float64 {
	var mx float64
	for _, c := range r.Cells {
		if v := cellMetric(c, metric); v > mx {
			mx = v
		}
	}
	return mx
}
