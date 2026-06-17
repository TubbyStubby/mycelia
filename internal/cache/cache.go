// Package cache holds parsed+merged per-group aggregations in memory, keyed by
// group identity, with signature-based invalidation and singleflight dedup.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"sync"

	"golang.org/x/sync/singleflight"

	"github.com/TubbyStubby/mycelia/internal/profiles"
	"github.com/TubbyStubby/mycelia/internal/v8profile"
)

// Cache stores merged aggregations per group. An entry is reused while the
// group's member signature is unchanged.
type Cache struct {
	mu    sync.RWMutex
	items map[string]*entry
	group singleflight.Group
}

type entry struct {
	agg *v8profile.Aggregation
	sig string
}

// New creates an empty cache.
func New() *Cache {
	return &Cache{items: map[string]*entry{}}
}

// MemberSignature derives a stable signature from a group's members (their raw
// names and sizes), so new uploads/objects invalidate the cached aggregation.
func MemberSignature(g profiles.Group) string {
	parts := make([]string, len(g.Members))
	for i, m := range g.Members {
		parts[i] = m.Key.Raw + ":" + strconv.FormatInt(m.Size, 10)
	}
	sort.Strings(parts)
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Get returns the cached aggregation for id when its signature matches sig.
func (c *Cache) Get(id profiles.GroupID, sig string) (*v8profile.Aggregation, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e, ok := c.items[id.String()]; ok && e.sig == sig {
		return e.agg, true
	}
	return nil, false
}

// Put stores an aggregation for id under sig.
func (c *Cache) Put(id profiles.GroupID, sig string, agg *v8profile.Aggregation) {
	c.mu.Lock()
	c.items[id.String()] = &entry{agg: agg, sig: sig}
	c.mu.Unlock()
}

// GetOrBuild returns the cached aggregation for id when its signature matches
// sig, otherwise it invokes build (deduped via singleflight) and caches the
// result.
func (c *Cache) GetOrBuild(id profiles.GroupID, sig string, build func() (*v8profile.Aggregation, error)) (*v8profile.Aggregation, error) {
	key := id.String()

	c.mu.RLock()
	if e, ok := c.items[key]; ok && e.sig == sig {
		agg := e.agg
		c.mu.RUnlock()
		return agg, nil
	}
	c.mu.RUnlock()

	v, err, _ := c.group.Do(key+"@"+sig, func() (any, error) {
		// Re-check after acquiring the singleflight slot.
		c.mu.RLock()
		if e, ok := c.items[key]; ok && e.sig == sig {
			agg := e.agg
			c.mu.RUnlock()
			return agg, nil
		}
		c.mu.RUnlock()

		agg, err := build()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.items[key] = &entry{agg: agg, sig: sig}
		c.mu.Unlock()
		return agg, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*v8profile.Aggregation), nil
}
