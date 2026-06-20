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
		mergeEdges(&out.Edges, a.Edges)
		if len(a.Contexts) > 0 {
			if out.Contexts == nil {
				out.Contexts = make(map[string]*Entity)
			}
			mergeEntities(out.Contexts, a.Contexts)
		}
		mergeEdges(&out.FunctionContexts, a.FunctionContexts)
		mergeEdges(&out.ContextPackages, a.ContextPackages)
		mergeEdges(&out.ContextFiles, a.ContextFiles)

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

// mergeEdges folds one aggregation's call-graph edges into the destination,
// allocating the destination map lazily so a group with no edges stays nil.
func mergeEdges(dst *map[string]map[string]Metric, src map[string]map[string]Metric) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[string]map[string]Metric, len(src))
	}
	for caller, callees := range src {
		row := (*dst)[caller]
		if row == nil {
			row = make(map[string]Metric, len(callees))
			(*dst)[caller] = row
		}
		for callee, m := range callees {
			cur := row[callee]
			cur.add(m)
			row[callee] = cur
		}
	}
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
