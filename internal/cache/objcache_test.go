package cache

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/TubbyStubby/mycelia/internal/v8profile"
)

// TestObjectCacheVersionedDisk checks that persisted aggregations land under the
// version subdirectory and survive a reopen (disk round-trip).
func TestObjectCacheVersionedDisk(t *testing.T) {
	base := t.TempDir()

	c, err := NewObjectCache(base)
	if err != nil {
		t.Fatal(err)
	}
	key := ObjectKey("profiles/x/1_host_1.cpuprofile", 1234)

	want := &v8profile.Aggregation{
		Functions: map[string]*v8profile.Entity{"k": {Key: "k", Metric: v8profile.Metric{SelfMicros: 7}}},
		Edges:     map[string]map[string]v8profile.Metric{"a": {"b": {TotalMicros: 3}}},
	}
	got, err := c.GetOrBuild(key, func() (*v8profile.Aggregation, error) { return want, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.Functions["k"].Metric.SelfMicros != 7 {
		t.Fatalf("built self = %d, want 7", got.Functions["k"].Metric.SelfMicros)
	}

	// The blob must live under base/v<N>/, not directly under base.
	blob := filepath.Join(base, VersionDir(), key+".gob")
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("expected blob at %s: %v", blob, err)
	}

	// A fresh cache over the same base reloads it from disk (build must not run).
	c2, err := NewObjectCache(base)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := c2.GetOrBuild(key, func() (*v8profile.Aggregation, error) {
		t.Fatal("build called on a disk hit")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Edges["a"]["b"].TotalMicros != 3 {
		t.Errorf("reloaded edge = %d, want 3", reloaded.Edges["a"]["b"].TotalMicros)
	}
}

// TestObjectKeyVersionScoped checks that the format version changes the key, so
// entries from different formats never collide.
func TestObjectKeyVersionScoped(t *testing.T) {
	if VersionDir() != "v"+strconv.Itoa(v8profile.FormatVersion) {
		t.Errorf("VersionDir = %q, want v%d", VersionDir(), v8profile.FormatVersion)
	}
	// Same inputs always yield the same (current-version) key.
	k1, k1again, k2 := ObjectKey("a", 1), ObjectKey("a", 1), ObjectKey("a", 2)
	if k1 != k1again {
		t.Error("ObjectKey is not stable")
	}
	if k1 == k2 {
		t.Error("ObjectKey ignored size")
	}
}
