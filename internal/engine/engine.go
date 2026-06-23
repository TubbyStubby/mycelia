// Package engine holds the profile orchestration shared by the HTTP API and the
// MCP server: selecting a source, browsing the hierarchy, and building per-group
// aggregations and N-group comparison matrices on top of the cache layers.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/TubbyStubby/mycelia/internal/cache"
	"github.com/TubbyStubby/mycelia/internal/compare"
	"github.com/TubbyStubby/mycelia/internal/config"
	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/store"
	"github.com/TubbyStubby/mycelia/internal/v8profile"
)

// ProgressReporter tracks how many profiles have been processed and emits an
// update on each advance. A nil reporter is a no-op.
type ProgressReporter struct {
	mu    sync.Mutex
	done  int
	total int
	emit  func(done, total int)
}

// NewProgressReporter returns a reporter that calls emit on each advance, having
// been initialized to a known total.
func NewProgressReporter(total int, emit func(done, total int)) *ProgressReporter {
	return &ProgressReporter{total: total, emit: emit}
}

func (p *ProgressReporter) add(n int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.done += n
	d, t := p.done, p.total
	p.mu.Unlock()
	if p.emit != nil {
		p.emit(d, t)
	}
}

// Engine owns the profile sources, caches, and configuration and exposes the
// browse/aggregate/compare operations built on top of them.
type Engine struct {
	cfg      config.Config
	gcs      *store.GCSSource // may be nil when GCS is not configured
	uploads  *store.UploadSource
	cache    *cache.Cache
	objCache *cache.ObjectCache
}

// New builds an Engine. gcs may be nil if the bucket/key were not configured.
func New(cfg config.Config, gcs *store.GCSSource, uploads *store.UploadSource, c *cache.Cache, oc *cache.ObjectCache) *Engine {
	return &Engine{cfg: cfg, gcs: gcs, uploads: uploads, cache: c, objCache: oc}
}

// GCSEnabled reports whether the engine has a configured GCS source.
func (e *Engine) GCSEnabled() bool { return e.gcs != nil }

// AddUpload stores manually uploaded profile files as a group and returns it.
func (e *Engine) AddUpload(id profiles.GroupID, files []store.NamedBytes) (profiles.Group, error) {
	return e.uploads.Add(id, files)
}

// sourceFor selects the source that owns a group.
func (e *Engine) sourceFor(id profiles.GroupID) (store.ProfileSource, error) {
	if id.Env == store.UploadEnv || e.uploads.Has(id) {
		return e.uploads, nil
	}
	if e.gcs == nil {
		return nil, fmt.Errorf("GCS is not configured; only uploaded groups are available")
	}
	return e.gcs, nil
}

// Browse lists the next hierarchy level for the filter. When includeUploads is
// set, upload groups are surfaced: an uploads-only view when GCS is absent or
// explicitly requested, otherwise uploads appear as a virtual env at the top
// level.
func (e *Engine) Browse(ctx context.Context, filter profiles.GroupFilter, includeUploads bool) (store.BrowseResult, error) {
	// Uploads-only view (no GCS configured, or explicitly requested).
	if filter.Env == store.UploadEnv || e.gcs == nil {
		return e.uploads.Browse(ctx, filter)
	}

	res, err := e.gcs.Browse(ctx, filter)
	if err != nil {
		return store.BrowseResult{}, err
	}

	// At the top level, surface uploads as a virtual environment so users can
	// reach uploaded groups through the same hierarchy.
	if includeUploads && filter.Env == "" {
		if ups, _ := e.uploads.ListGroups(ctx, profiles.GroupFilter{}); len(ups) > 0 {
			res.Children = append(res.Children, store.UploadEnv)
		}
	}
	return res, nil
}

// Members returns a group's member objects (empty if the group is unknown).
func (e *Engine) Members(ctx context.Context, id profiles.GroupID) ([]profiles.GroupMember, error) {
	src, err := e.sourceFor(id)
	if err != nil {
		return nil, err
	}
	groups, err := src.ListGroups(ctx, profiles.GroupFilter{
		Env: id.Env, Service: id.Service, Date: id.Date, BuildTag: id.BuildTag,
	})
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.ID == id {
			return g.Members, nil
		}
	}
	return nil, nil
}

// GroupAggregation lists a group's members, samples them, then returns the
// cached merged aggregation (rebuilding when the sampled set changes). It also
// reports the total number of profiles in the group before sampling.
func (e *Engine) GroupAggregation(ctx context.Context, id profiles.GroupID) (*v8profile.Aggregation, int, error) {
	p, err := e.planGroup(ctx, id, Window{})
	if err != nil {
		return nil, 0, err
	}
	agg, err := e.cache.GetOrBuild(id, p.sig, func() (*v8profile.Aggregation, error) {
		return e.buildGroup(ctx, p.src, p.sampled, nil)
	})
	return agg, p.total, err
}

// ErrEntityNotFound is returned by FunctionBreakdown / EntityBreakdown when the
// requested key is absent from the group's aggregation for that dimension. It is
// a client-level condition (e.g. drilling an entity present in one group but not
// another), so callers can distinguish it from upstream fetch failures.
var ErrEntityNotFound = errors.New("entity not found")

// ErrNoBuilds is returned by ResolveLatest when no groups match the supplied
// env/service (and optional date) filter, so the caller can produce an
// actionable error message without inspecting the string.
var ErrNoBuilds = errors.New("no builds found")

// ResolveLatest resolves the "latest" buildTag sentinel to the concrete GroupID
// whose newest member timestamp is greatest. If id.BuildTag is not "latest" the
// call is a no-op and id is returned unchanged.
//
// Resolution scope: if id.Date is a concrete date (non-empty and not "latest")
// the search is restricted to that date; otherwise all dates for the service are
// searched. Among the matching groups the one with the newest member timestamp
// wins; ties break by Date then BuildTag descending so the result is stable even
// when timestamps are absent or equal.
func (e *Engine) ResolveLatest(ctx context.Context, id profiles.GroupID) (profiles.GroupID, error) {
	if id.BuildTag != "latest" {
		return id, nil
	}

	src, err := e.sourceFor(id)
	if err != nil {
		return profiles.GroupID{}, err
	}

	filter := profiles.GroupFilter{Env: id.Env, Service: id.Service}
	if id.Date != "" && id.Date != "latest" {
		filter.Date = id.Date
	}

	groups, err := src.ListGroups(ctx, filter)
	if err != nil {
		return profiles.GroupID{}, err
	}
	if len(groups) == 0 {
		return profiles.GroupID{}, fmt.Errorf("%w for %s/%s", ErrNoBuilds, id.Env, id.Service)
	}

	return pickNewestGroup(groups), nil
}

// pickNewestGroup returns the GroupID of the group whose newest member
// timestamp is greatest. Ties break by Date then BuildTag descending so the
// result is stable even when timestamps are absent or equal.
func pickNewestGroup(groups []profiles.Group) profiles.GroupID {
	best := groups[0]
	bestMax := newestMemberTs(best.Members)

	for _, g := range groups[1:] {
		gMax := newestMemberTs(g.Members)
		if gMax.After(bestMax) {
			best = g
			bestMax = gMax
			continue
		}
		if gMax.Equal(bestMax) {
			// Stable tiebreak: prefer later date, then later buildTag (both
			// descending lexicographic, which matches YYYY-MM-DD ordering).
			if g.ID.Date > best.ID.Date ||
				(g.ID.Date == best.ID.Date && g.ID.BuildTag > best.ID.BuildTag) {
				best = g
				// bestMax unchanged — timestamps are equal
			}
		}
	}
	return best.ID
}

// newestMemberTs returns the maximum Timestamp across a group's members.
// Zero is returned when the group has no members or no member carries a
// non-zero timestamp.
func newestMemberTs(members []profiles.GroupMember) time.Time {
	var max time.Time
	for _, m := range members {
		if m.Key.Timestamp.After(max) {
			max = m.Key.Timestamp
		}
	}
	return max
}

// FunctionBreakdown returns the immediate callers and callees of fnKey within a
// group, ranked by inclusive cost and capped at topN (0 = all). fnKey is a
// function key as returned in a compare Row's Key field. When stitch is set,
// caller edges are attributed through transparent async/native trampoline frames
// to the nearest meaningful ancestor (marked ViaAsync). ctxSort orders the
// returned Contexts list (see compare.ContextSort; zero value = by micros).
// categories restricts edges to the given categories (native|node_modules|user|
// idle); an empty slice includes all. Context-label rows are never filtered.
func (e *Engine) FunctionBreakdown(ctx context.Context, id profiles.GroupID, fnKey string, topN int, stitch bool, ctxSort compare.ContextSort, categories []string) (compare.Breakdown, error) {
	return e.EntityBreakdown(ctx, id, compare.DimFunction, fnKey, topN, stitch, ctxSort, categories)
}

// EntityBreakdown drills one entity within a group: a function (callers/callees/
// contexts), a package (its functions/files/contexts), a file (its functions/
// contexts), or a context (its functions/packages/files). key is the entity's
// Key from a compare Row; topN caps each section (0 = all). stitch and ctxSort
// apply only to the function dimension. categories restricts entity rows to the
// given categories; an empty slice includes all. Context-label rows are never
// filtered. Returns ErrEntityNotFound when key is absent for dim.
func (e *Engine) EntityBreakdown(ctx context.Context, id profiles.GroupID, dim compare.Dimension, key string, topN int, stitch bool, ctxSort compare.ContextSort, categories []string) (compare.Breakdown, error) {
	agg, _, err := e.GroupAggregation(ctx, id)
	if err != nil {
		return compare.Breakdown{}, err
	}
	bd, ok := compare.BuildEntityBreakdown(agg, dim, key, topN, stitch, ctxSort, allowedSet(categories))
	if !ok {
		return compare.Breakdown{}, fmt.Errorf("%w: %s %q in group %s", ErrEntityNotFound, dimLabel(dim), key, id)
	}
	return bd, nil
}

// dimLabel names a dimension for error messages, defaulting to function.
func dimLabel(dim compare.Dimension) string {
	if dim == "" {
		return string(compare.DimFunction)
	}
	return string(dim)
}

// Window bounds a group's members by member timestamp. A zero From or To is
// unbounded on that end; a zero Window includes every member.
type Window struct {
	From time.Time
	To   time.Time
}

// CompareOptions configures a comparison. Zero values select the defaults
// (function dimension, selfMicros metric, max sort, no category/time filter).
type CompareOptions struct {
	Dimension  compare.Dimension
	Metric     compare.Metric
	TopN       int
	Categories []string
	Sort       compare.SortMode
	Window     Window
}

// Compare lists, samples, and builds each group, then assembles a comparison
// matrix. prog may be nil; when set it is advanced per processed profile (after
// the total work is known). opts.Categories restricts rows to the given filter
// categories (native|node_modules|user|idle); an empty slice includes all.
func (e *Engine) Compare(ctx context.Context, ids []profiles.GroupID, opts CompareOptions, prog *ProgressReporter) (compare.Matrix, error) {
	if len(ids) == 0 {
		return compare.Matrix{}, fmt.Errorf("at least one group is required")
	}
	dim := opts.Dimension
	if dim == "" {
		dim = compare.DimFunction
	}

	// Plan: list + sample each group so the total work is known up front.
	plans := make([]groupPlan, 0, len(ids))
	for _, id := range ids {
		p, err := e.planGroup(ctx, id, opts.Window)
		if err != nil {
			return compare.Matrix{}, fmt.Errorf("group %s: %w", id, err)
		}
		plans = append(plans, p)
	}
	if prog != nil {
		total := 0
		for _, p := range plans {
			total += len(p.sampled)
		}
		prog.total = total
		prog.add(0) // emit an initial 0/total
	}

	// Build groups sequentially so concurrency stays bounded by
	// FetchConcurrency within each group and progress advances smoothly.
	aggs := make([]compare.GroupAggregation, len(plans))
	for i, p := range plans {
		agg, err := e.buildPlan(ctx, p, prog)
		if err != nil {
			return compare.Matrix{}, fmt.Errorf("group %s: %w", p.id, err)
		}
		aggs[i] = compare.GroupAggregation{
			ID: p.id, Agg: agg, TotalProfiles: p.total,
			FirstTs: p.firstTs, LastTs: p.lastTs,
		}
	}

	return compare.BuildMatrix(aggs, dim, opts.Metric, opts.TopN, allowedSet(opts.Categories), opts.Sort), nil
}

// groupPlan is a group resolved to its sampled members, ready to build.
type groupPlan struct {
	id              profiles.GroupID
	src             store.ProfileSource
	sampled         []profiles.GroupMember
	sig             string
	total           int       // group size after window filtering, before sampling
	firstTs, lastTs time.Time // timestamp span of the sampled members
}

// planGroup lists and samples a group's members without downloading them. When
// win is non-zero, members outside the window are dropped before sampling so the
// aggregation reflects only that wall-clock slice (and caches under a distinct
// member signature).
func (e *Engine) planGroup(ctx context.Context, id profiles.GroupID, win Window) (groupPlan, error) {
	src, err := e.sourceFor(id)
	if err != nil {
		return groupPlan{}, err
	}
	groups, err := src.ListGroups(ctx, profiles.GroupFilter{
		Env: id.Env, Service: id.Service, Date: id.Date, BuildTag: id.BuildTag,
	})
	if err != nil {
		return groupPlan{}, err
	}
	var group profiles.Group
	for _, g := range groups {
		if g.ID == id {
			group = g
			break
		}
	}
	if len(group.Members) == 0 {
		return groupPlan{}, fmt.Errorf("group %q has no profiles", id)
	}
	members := filterWindow(group.Members, win)
	if len(members) == 0 {
		return groupPlan{}, fmt.Errorf("group %q has no profiles in the requested time window", id)
	}
	sampled := profiles.Sample(members, e.cfg.SampleSize)
	first, last := timeSpan(sampled)
	return groupPlan{
		id:      id,
		src:     src,
		sampled: sampled,
		sig:     cache.MemberSignature(profiles.Group{ID: id, Members: sampled}),
		total:   len(members),
		firstTs: first,
		lastTs:  last,
	}, nil
}

// filterWindow keeps members whose timestamp falls within win. A zero From/To
// is unbounded on that end; a zero window returns the members unchanged.
func filterWindow(members []profiles.GroupMember, win Window) []profiles.GroupMember {
	if win.From.IsZero() && win.To.IsZero() {
		return members
	}
	out := members[:0:0]
	for _, m := range members {
		ts := m.Key.Timestamp
		if !win.From.IsZero() && ts.Before(win.From) {
			continue
		}
		if !win.To.IsZero() && ts.After(win.To) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// timeSpan returns the earliest and latest member timestamps (zero if none).
func timeSpan(members []profiles.GroupMember) (first, last time.Time) {
	for _, m := range members {
		ts := m.Key.Timestamp
		if ts.IsZero() {
			continue
		}
		if first.IsZero() || ts.Before(first) {
			first = ts
		}
		if last.IsZero() || ts.After(last) {
			last = ts
		}
	}
	return first, last
}

// buildPlan returns a planned group's aggregation, using the cache when warm and
// reporting per-profile progress otherwise.
func (e *Engine) buildPlan(ctx context.Context, p groupPlan, prog *ProgressReporter) (*v8profile.Aggregation, error) {
	if agg, ok := e.cache.Get(p.id, p.sig); ok {
		prog.add(len(p.sampled)) // already processed; advance the bar
		return agg, nil
	}
	agg, err := e.buildGroup(ctx, p.src, p.sampled, prog)
	if err != nil {
		return nil, err
	}
	e.cache.Put(p.id, p.sig, agg)
	return agg, nil
}

// buildGroup fetches, parses, and aggregates each member profile (reusing the
// per-object cache for immutable objects) and merges the results. Concurrency
// is bounded by FetchConcurrency since the work is largely network-bound. prog
// may be nil; when set, it is advanced by one per processed profile.
func (e *Engine) buildGroup(ctx context.Context, src store.ProfileSource, members []profiles.GroupMember, prog *ProgressReporter) (*v8profile.Aggregation, error) {
	aggs := make([]*v8profile.Aggregation, len(members))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(max(2, e.cfg.FetchConcurrency))

	for i, m := range members {
		i, m := i, m
		g.Go(func() error {
			key := cache.ObjectKey(m.Key.Raw, m.Size, e.cfg.BlockThresholdMicros)
			a, err := e.objCache.GetOrBuild(key, func() (*v8profile.Aggregation, error) {
				rc, err := src.OpenMember(ctx, m.Key)
				if err != nil {
					return nil, fmt.Errorf("open %s: %w", m.Key.Raw, err)
				}
				defer rc.Close()

				prof, err := v8profile.ParseProfile(rc)
				if err != nil {
					return nil, fmt.Errorf("parse %s: %w", m.Key.Raw, err)
				}
				// Drain anything left so the GCS reader closes cleanly.
				_, _ = io.Copy(io.Discard, rc)
				return v8profile.AggregateProfileWithThreshold(prof, e.cfg.BlockThresholdMicros), nil
			})
			if err != nil {
				return err
			}
			aggs[i] = a
			prog.add(1)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return v8profile.MergeAggregations(aggs...), nil
}

// allowedSet converts a category list to a lookup set. An empty list returns
// nil, meaning "all categories".
func allowedSet(categories []string) map[string]bool {
	if len(categories) == 0 {
		return nil
	}
	set := make(map[string]bool, len(categories))
	for _, c := range categories {
		set[c] = true
	}
	return set
}
