// Package gcache provides a local cache plugin backed by github.com/bluele/gcache.
//
// Concurrency: the underlying library guards every operation with a single
// global mutex, so concurrent reads are fully serialized — this plugin only
// fits low-concurrency, small-data workloads. For high-concurrency reads
// prefer freecache or bigcache.
//
// Capability boundaries:
//   - Does NOT implement cache.PatternStore: DeletePattern is not supported
//     and silently falls back to a no-op in the cache package.
//   - Does NOT implement cache.BulkStore: GetMulti/SetMulti fall back to
//     individual Get/Put calls in the cache package.
//   - cache.Options fields slidingWindow, ttlJitter, maxItems and maxBytes are
//     only honored by the built-in memory store (mem_store); switching to this
//     plugin silently degrades those capabilities.
//   - The cache package version-sync feature (cache.WithVersionSyncInterval)
//     and probabilistic verification (cache.WithVerifyEvery) only operate on
//     the built-in memory store and are silently disabled with this plugin.
package gcache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bluele/gcache"
)

// gcache_store adapts a github.com/bluele/gcache.Cache to cache.Store.
type gcache_store struct {
	s gcache.Cache
}

// newGcache builds a size-bounded LRU cache. size must be > 0 because the
// underlying library panics in Build() when size <= 0.
func newGcache(size int) (gcache_store, error) {
	if size <= 0 {
		return gcache_store{}, fmt.Errorf("gcache: size must be > 0, got %d", size)
	}

	c := gcache.New(size).
		LRU().
		Build()

	return gcache_store{s: c}, nil
}

// Get returns the cached value for key. A missing key yields (nil, false, nil).
func (c gcache_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	r, err := c.s.Get(key)
	if err != nil {
		if errors.Is(err, gcache.KeyNotFoundError) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("gcache get key=%s: %w", key, err)
	}

	v, ok := r.([]byte)
	if !ok {
		return nil, false, fmt.Errorf("gcache get key=%s: unexpected cached value type %T", key, r)
	}

	return v, true, nil
}

// Put stores a key-value pair. expireSeconds <= 0 means no expiration, matching
// the cache.Store contract (see cache/memory_store.go).
func (c gcache_store) Put(ctx context.Context, key string, v []byte, expireSeconds int) error {
	if expireSeconds <= 0 {
		// The underlying LRUCache.set only updates the value of an existing
		// entry and does not reset its per-key expiration field. Remove the old
		// entry first so an earlier Put with a positive TTL cannot leave a
		// stale expiration behind that would delete this entry later.
		c.s.Remove(key)
		return c.s.Set(key, v)
	}

	return c.s.SetWithExpire(key, v, time.Second*time.Duration(expireSeconds))
}

// Delete removes keys. It is idempotent: removing a missing key is a no-op.
func (c gcache_store) Delete(ctx context.Context, keys ...string) error {
	for _, k := range keys {
		c.s.Remove(k)
	}

	return nil
}

func (c gcache_store) IsRemote() bool { return false }
func (gcache_store) Name() string     { return "gcache" }
