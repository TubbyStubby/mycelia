package v8profile

// MergeAggregations folds several per-profile aggregations into one combined
// aggregation, summing entity metrics key-by-key. It is associative, so callers
// may fold in any order. The returned aggregation has ProfileCount equal to the
// number of non-nil inputs.
func MergeAggregations(aggs ...*Aggregation) *Aggregation {
	out := &Aggregation{
		Functions: make(map[string]*Entity),
		Files:     make(map[string]*Entity),
		Packages:  make(map[string]*Entity),
	}

	for _, a := range aggs {
		if a == nil {
			continue
		}
		mergeEntities(out.Functions, a.Functions)
		mergeEntities(out.Files, a.Files)
		mergeEntities(out.Packages, a.Packages)

		out.Overall.add(a.Overall)
		out.DurationMicros += a.DurationMicros
		out.SampleCount += a.SampleCount
		out.ProfileCount += a.ProfileCount
		if a.TimingApproximate {
			out.TimingApproximate = true
		}
	}

	return out
}

func mergeEntities(dst, src map[string]*Entity) {
	for key, e := range src {
		d := dst[key]
		if d == nil {
			cp := *e
			cp.Metric = Metric{}
			d = &cp
			dst[key] = d
		}
		d.Metric.add(e.Metric)
	}
}
