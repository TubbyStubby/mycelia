package cache

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"golang.org/x/sync/singleflight"

	"github.com/TubbyStubby/mycelia/internal/v8profile"
)

// ObjectCache caches per-object (per-file) aggregations. GCS objects are
// immutable, so a given object name+size only ever needs to be downloaded and
// parsed once. When dir is non-empty, aggregations are also persisted to disk
// so they survive restarts.
type ObjectCache struct {
	mu  sync.RWMutex
	mem map[string]*v8profile.Aggregation
	sf  singleflight.Group
	dir string
}

// NewObjectCache creates a per-object cache. dir may be empty (memory only). If
// dir is set it is created if missing.
func NewObjectCache(dir string) (*ObjectCache, error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return &ObjectCache{mem: map[string]*v8profile.Aggregation{}, dir: dir}, nil
}

// ObjectKey derives a stable cache key from an object's name and size.
func ObjectKey(name string, size int64) string {
	sum := sha256.Sum256([]byte(name + ":" + strconv.FormatInt(size, 10)))
	return hex.EncodeToString(sum[:])
}

// GetOrBuild returns the cached per-object aggregation for key, building it via
// build on a miss. Concurrent requests for the same key are deduplicated.
func (c *ObjectCache) GetOrBuild(key string, build func() (*v8profile.Aggregation, error)) (*v8profile.Aggregation, error) {
	c.mu.RLock()
	if a, ok := c.mem[key]; ok {
		c.mu.RUnlock()
		return a, nil
	}
	c.mu.RUnlock()

	v, err, _ := c.sf.Do(key, func() (any, error) {
		c.mu.RLock()
		if a, ok := c.mem[key]; ok {
			c.mu.RUnlock()
			return a, nil
		}
		c.mu.RUnlock()

		if a := c.loadDisk(key); a != nil {
			c.store(key, a, false)
			return a, nil
		}

		a, err := build()
		if err != nil {
			return nil, err
		}
		c.store(key, a, true)
		return a, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*v8profile.Aggregation), nil
}

func (c *ObjectCache) store(key string, a *v8profile.Aggregation, persist bool) {
	c.mu.Lock()
	c.mem[key] = a
	c.mu.Unlock()
	if persist && c.dir != "" {
		c.saveDisk(key, a)
	}
}

func (c *ObjectCache) path(key string) string {
	return filepath.Join(c.dir, key+".gob")
}

func (c *ObjectCache) loadDisk(key string) *v8profile.Aggregation {
	if c.dir == "" {
		return nil
	}
	f, err := os.Open(c.path(key))
	if err != nil {
		return nil
	}
	defer f.Close()
	var a v8profile.Aggregation
	if err := gob.NewDecoder(f).Decode(&a); err != nil {
		return nil
	}
	return &a
}

func (c *ObjectCache) saveDisk(key string, a *v8profile.Aggregation) {
	tmp := c.path(key) + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	if err := gob.NewEncoder(f).Encode(a); err != nil {
		f.Close()
		os.Remove(tmp)
		return
	}
	f.Close()
	_ = os.Rename(tmp, c.path(key))
}
