package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/store"
)

// grp builds a group with the given identity and one member per timestamp.
func grp(date, build string, ts ...time.Time) profiles.Group {
	g := profiles.Group{ID: profiles.GroupID{Env: "prod", Service: "web", Date: date, BuildTag: build}}
	for _, t := range ts {
		g.Members = append(g.Members, profiles.GroupMember{Key: profiles.ObjectKey{Timestamp: t}})
	}
	return g
}

// TestPickNewestGroup checks the newest member timestamp wins regardless of
// slice order or how many profiles each build holds.
func TestPickNewestGroup(t *testing.T) {
	t0 := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	groups := []profiles.Group{
		grp("2026-06-18", "old", t0, t0.Add(time.Hour)),
		grp("2026-06-20", "new", t0.Add(48*time.Hour)),
		grp("2026-06-19", "mid", t0.Add(24*time.Hour), t0.Add(25*time.Hour)),
	}
	if got := pickNewestGroup(groups); got.BuildTag != "new" {
		t.Errorf("pickNewestGroup = %q, want new", got.BuildTag)
	}
}

// TestPickNewestGroupTiebreak checks that when timestamps are equal (or absent)
// the result is stable: later date wins, then later buildTag.
func TestPickNewestGroupTiebreak(t *testing.T) {
	ts := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)
	groups := []profiles.Group{
		grp("2026-06-20", "aaa", ts),
		grp("2026-06-19", "zzz", ts),
		grp("2026-06-20", "zzz", ts),
	}
	got := pickNewestGroup(groups)
	if got.Date != "2026-06-20" || got.BuildTag != "zzz" {
		t.Errorf("tiebreak = %s/%s, want 2026-06-20/zzz", got.Date, got.BuildTag)
	}
}

// TestNewestMemberTs checks the max-timestamp helper, including the no-members
// (zero time) case.
func TestNewestMemberTs(t *testing.T) {
	if got := newestMemberTs(nil); !got.IsZero() {
		t.Errorf("newestMemberTs(nil) = %v, want zero", got)
	}
	t0 := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	g := grp("d", "b", t0, t0.Add(2*time.Hour), t0.Add(time.Hour))
	if got := newestMemberTs(g.Members); !got.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("newestMemberTs = %v, want %v", got, t0.Add(2*time.Hour))
	}
}

// TestResolveLatestNoop checks a concrete buildTag is returned unchanged without
// touching any source (the fast path).
func TestResolveLatestNoop(t *testing.T) {
	id := profiles.GroupID{Env: "prod", Service: "web", Date: "2026-06-20", BuildTag: "abc123"}
	got, err := (&Engine{}).ResolveLatest(context.Background(), id)
	if err != nil || got != id {
		t.Errorf("ResolveLatest noop = (%v, %v), want (%v, nil)", got, err, id)
	}
}

// TestResolveLatestNoBuilds checks the error path: a "latest" request against a
// service with no groups reports ErrNoBuilds.
func TestResolveLatestNoBuilds(t *testing.T) {
	e := &Engine{uploads: store.NewUploadSource()}
	id := profiles.GroupID{Env: store.UploadEnv, Service: "web", BuildTag: "latest"}
	if _, err := e.ResolveLatest(context.Background(), id); !errors.Is(err, ErrNoBuilds) {
		t.Errorf("ResolveLatest err = %v, want ErrNoBuilds", err)
	}
}
