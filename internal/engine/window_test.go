package engine

import (
	"testing"
	"time"

	"github.com/TubbyStubby/mycelia/internal/profiles"
)

func member(ts string) profiles.GroupMember {
	t, _ := time.Parse(time.RFC3339, ts)
	return profiles.GroupMember{Key: profiles.ObjectKey{Timestamp: t}}
}

func TestFilterWindow(t *testing.T) {
	members := []profiles.GroupMember{
		member("2026-06-17T12:00:00Z"),
		member("2026-06-17T14:00:00Z"),
		member("2026-06-17T16:00:00Z"),
	}

	// Zero window is a pass-through.
	if got := filterWindow(members, Window{}); len(got) != 3 {
		t.Errorf("zero window kept %d, want 3", len(got))
	}

	from, _ := time.Parse(time.RFC3339, "2026-06-17T13:00:00Z")
	to, _ := time.Parse(time.RFC3339, "2026-06-17T15:00:00Z")
	got := filterWindow(members, Window{From: from, To: to})
	if len(got) != 1 || !got[0].Key.Timestamp.Equal(members[1].Key.Timestamp) {
		t.Errorf("windowed = %+v, want only the 14:00 member", got)
	}

	// Filtering must not mutate the input slice's backing array.
	if !members[0].Key.Timestamp.Equal(member("2026-06-17T12:00:00Z").Key.Timestamp) {
		t.Errorf("input slice was mutated: %v", members[0].Key.Timestamp)
	}

	// Open-ended bounds.
	if got := filterWindow(members, Window{From: from}); len(got) != 2 {
		t.Errorf("from-only kept %d, want 2", len(got))
	}
	if got := filterWindow(members, Window{To: to}); len(got) != 2 {
		t.Errorf("to-only kept %d, want 2", len(got))
	}
}

func TestTimeSpan(t *testing.T) {
	members := []profiles.GroupMember{
		member("2026-06-17T16:00:00Z"),
		member("2026-06-17T12:00:00Z"),
		member("2026-06-17T14:00:00Z"),
	}
	first, last := timeSpan(members)
	if first.Format(time.RFC3339) != "2026-06-17T12:00:00Z" {
		t.Errorf("first = %v, want 12:00", first)
	}
	if last.Format(time.RFC3339) != "2026-06-17T16:00:00Z" {
		t.Errorf("last = %v, want 16:00", last)
	}

	if f, l := timeSpan(nil); !f.IsZero() || !l.IsZero() {
		t.Errorf("empty span = %v/%v, want zero/zero", f, l)
	}
}
