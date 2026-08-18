// Package freecache provides a local cache plugin backed by github.com/coocood/freecache.
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
package freecache

import (
	"context"
	"errors"
	"fmt"

	"github.com/coocood/freecache"
)

type freecache_store struct {
	cache *freecache.Cache
}

func new(size int) *freecache_store {
	c := freecache.NewCache(size)

	return &freecache_store{
		cache: c,
	}
}

func (f *freecache_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, err := f.cache.Get([]byte(key))
	if err != nil {
		if errors.Is(err, freecache.ErrNotFound) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("freecache get key=%s: %w", key, err)
	}

	return value, true, nil
}

func (f *freecache_store) Put(ctx context.Context, key string, v []byte, expireSeconds int) error {
	if err := f.cache.Set([]byte(key), v, expireSeconds); err != nil {
		return fmt.Errorf("freecache put key=%s: %w", key, err)
	}

	return nil
}

// Delete removes keys. It is idempotent: freecache.Del returns a bool (whether
// an entry was removed) and never errors, so deleting a missing key is a no-op.
func (f *freecache_store) Delete(ctx context.Context, key ...string) error {
	for _, k := range key {
		_ = f.cache.Del([]byte(k))
	}

	return nil
}

func (r *freecache_store) Name() string { return "freecache" }

func (*freecache_store) IsRemote() bool { return false }
