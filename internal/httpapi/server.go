// Package httpapi wires the profile sources, cache, and comparison engine into
// an HTTP server with a JSON API and an embedded static frontend.
package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/TubbyStubby/mycelia/internal/cache"
	"github.com/TubbyStubby/mycelia/internal/config"
	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/store"
	"github.com/TubbyStubby/mycelia/internal/v8profile"
	"github.com/TubbyStubby/mycelia/web"
)

// progressReporter tracks how many profiles have been processed and emits an
// update on each advance. A nil reporter is a no-op.
type progressReporter struct {
	mu    sync.Mutex
	done  int
	total int
	emit  func(done, total int)
}

func (p *progressReporter) add(n int) {
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

// Server holds the API dependencies.
type Server struct {
	cfg      config.Config
	gcs      *store.GCSSource // may be nil when GCS is not configured
	uploads  *store.UploadSource
	cache    *cache.Cache
	objCache *cache.ObjectCache
}

// New builds a Server. gcs may be nil if the bucket/key were not configured.
func New(cfg config.Config, gcs *store.GCSSource, uploads *store.UploadSource, c *cache.Cache, oc *cache.ObjectCache) *Server {
	return &Server{cfg: cfg, gcs: gcs, uploads: uploads, cache: c, objCache: oc}
}

// Handler returns the configured HTTP handler (routes + static assets).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/groups", s.handleGroups)
	mux.HandleFunc("GET /api/group/{env}/{service}/{date}/{buildTag}", s.handleGroup)
	mux.HandleFunc("POST /api/compare", s.handleCompare)
	mux.HandleFunc("POST /api/upload", s.handleUpload)

	mux.Handle("GET /", http.FileServerFS(web.FS()))
	return mux
}

// sourceFor selects the source that owns a group.
func (s *Server) sourceFor(id profiles.GroupID) (store.ProfileSource, error) {
	if id.Env == store.UploadEnv || s.uploads.Has(id) {
		return s.uploads, nil
	}
	if s.gcs == nil {
		return nil, fmt.Errorf("GCS is not configured; only uploaded groups are available")
	}
	return s.gcs, nil
}

// groupAggregation lists a group's members, samples them, then returns the
// cached merged aggregation (rebuilding when the sampled set changes). It also
// reports the total number of profiles in the group before sampling.
func (s *Server) groupAggregation(ctx context.Context, id profiles.GroupID) (*v8profile.Aggregation, int, error) {
	src, err := s.sourceFor(id)
	if err != nil {
		return nil, 0, err
	}

	groups, err := src.ListGroups(ctx, profiles.GroupFilter{
		Env: id.Env, Service: id.Service, Date: id.Date, BuildTag: id.BuildTag,
	})
	if err != nil {
		return nil, 0, err
	}
	var group profiles.Group
	for _, g := range groups {
		if g.ID == id {
			group = g
			break
		}
	}
	total := len(group.Members)
	if total == 0 {
		return nil, 0, fmt.Errorf("group %q has no profiles", id)
	}

	// Deterministically sample so the cache signature is stable and results are
	// reproducible.
	sampled := profiles.Sample(group.Members, s.cfg.SampleSize)
	sig := cache.MemberSignature(profiles.Group{ID: id, Members: sampled})
	agg, err := s.cache.GetOrBuild(id, sig, func() (*v8profile.Aggregation, error) {
		return s.buildGroup(ctx, src, sampled, nil)
	})
	return agg, total, err
}

// groupPlan is a group resolved to its sampled members, ready to build.
type groupPlan struct {
	id      profiles.GroupID
	src     store.ProfileSource
	sampled []profiles.GroupMember
	sig     string
	total   int // group size before sampling
}

// planGroup lists and samples a group's members without downloading them.
func (s *Server) planGroup(ctx context.Context, id profiles.GroupID) (groupPlan, error) {
	src, err := s.sourceFor(id)
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
	sampled := profiles.Sample(group.Members, s.cfg.SampleSize)
	return groupPlan{
		id:      id,
		src:     src,
		sampled: sampled,
		sig:     cache.MemberSignature(profiles.Group{ID: id, Members: sampled}),
		total:   len(group.Members),
	}, nil
}

// buildPlan returns a planned group's aggregation, using the cache when warm and
// reporting per-profile progress otherwise.
func (s *Server) buildPlan(ctx context.Context, p groupPlan, prog *progressReporter) (*v8profile.Aggregation, error) {
	if agg, ok := s.cache.Get(p.id, p.sig); ok {
		prog.add(len(p.sampled)) // already processed; advance the bar
		return agg, nil
	}
	agg, err := s.buildGroup(ctx, p.src, p.sampled, prog)
	if err != nil {
		return nil, err
	}
	s.cache.Put(p.id, p.sig, agg)
	return agg, nil
}

// buildGroup fetches, parses, and aggregates each member profile (reusing the
// per-object cache for immutable objects) and merges the results. Concurrency
// is bounded by FetchConcurrency since the work is largely network-bound. prog
// may be nil; when set, it is advanced by one per processed profile.
func (s *Server) buildGroup(ctx context.Context, src store.ProfileSource, members []profiles.GroupMember, prog *progressReporter) (*v8profile.Aggregation, error) {
	aggs := make([]*v8profile.Aggregation, len(members))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(max(2, s.cfg.FetchConcurrency))

	for i, m := range members {
		i, m := i, m
		g.Go(func() error {
			key := cache.ObjectKey(m.Key.Raw, m.Size)
			a, err := s.objCache.GetOrBuild(key, func() (*v8profile.Aggregation, error) {
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
				return v8profile.AggregateProfile(prof), nil
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
