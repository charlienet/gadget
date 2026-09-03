// Package bigcache provides a local cache plugin backed by github.com/allegro/bigcache/v3.
//
// A single store instance must only be installed into one cache instance:
// the Initialize hook and rebuild() close and recreate the underlying
// BigCache, so reusing the same store across several cache instances
// silently wipes previously cached entries.
//
// TTL semantics: bigcache does NOT support per-key TTL. All entries share a
// single global LifeWindow configured at construction time, so the cache.Store
// contract's expireSecond parameter (where <= 0 means "never expire", see
// cache/memory_store.go) is NOT honored per key: an entry written with
// expireSecond=0 still expires once LifeWindow elapses. To approximate "never
// expire", configure a large LifeWindow via WithLifeWindow. When the cache
// package is built with a global TTL (cache.WithTTL), the LifeWindow is linked
// to it through the Initialize hook.
//
// Oversized values: when an entry does not fit in a shard's byte queue, the
// underlying library evicts oldest entries until it fits — if the value alone
// exceeds the whole per-shard queue it evicts every entry of that shard before
// failing the write. Put() therefore pre-checks the value size against the
// per-shard limit (HardMaxCacheSize/Shards, by default 256MB/128 = 2MB) and
// returns an explicit error instead of calling the library.
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
package bigcache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/charlienet/gadget/cache"
)

// Default configuration values. They deliberately deviate from the underlying
// library defaults: bigcache.New with DefaultConfig pre-allocates per-shard
// byte queues sized by Shards x MaxEntriesInWindow x MaxEntrySize (about 286MB
// at 1024 shards) and leaves HardMaxCacheSize at 0 (unlimited). Our defaults
// cap the hard limit at 256MB and keep the pre-allocation around 37MB.
const (
	defaultHardMaxCacheSizeMB = 256         // hard memory limit in MB; 0 = unlimited
	defaultShards             = 128         // must be a power of two
	defaultLifeWindow         = time.Minute // global entry lifetime
	defaultCleanWindow        = time.Minute // background cleanup interval
	defaultMaxEntrySize       = 500         // bytes
	defaultMaxEntriesInWindow = 585 * 128   // ~37MB pre-allocation (entries x entry size)
)

// bigcache_store adapts a *bigcache.BigCache to cache.Store.
type bigcache_store struct {
	cache  *bigcache.BigCache
	config bigcache.Config
	// closed is set by Close; it lets tests verify that the cache package's
	// Close() actually reaches this store.
	closed bool
}

// NewBigCache builds a bigcache-backed store with safe defaults: a 256MB hard
// cap, 128 shards and a 1-minute global LifeWindow. Optional config funcs
// override individual settings. It returns an error when the combined
// configuration fails validation (see validateConfig) or when the underlying
// library rejects the configuration.
//
// Note: this constructor shares its name with the underlying library's
// bigcache.New but is a distinct adapter API returning a cache.Store — do not
// confuse the two.
func NewBigCache(configs ...ConfigFunc) (*bigcache_store, error) {
	config := bigcache.Config{
		Shards:             defaultShards,
		LifeWindow:         defaultLifeWindow,
		CleanWindow:        defaultCleanWindow,
		MaxEntriesInWindow: defaultMaxEntriesInWindow,
		MaxEntrySize:       defaultMaxEntrySize,
		StatsEnabled:       false,
		Verbose:            false,
		HardMaxCacheSize:   defaultHardMaxCacheSizeMB,
	}

	for _, apply := range configs {
		apply(&config)
	}

	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("bigcache: invalid config: %w", err)
	}

	s := &bigcache_store{config: config}
	if err := s.rebuild(); err != nil {
		return nil, err
	}

	return s, nil
}

// validateConfig rejects configuration values that would silently misbehave
// in the underlying library: a LifeWindow below 1s is truncated to 0 by
// bigcache's per-second resolution (every cleanup would then wipe the whole
// cache), a CleanWindow <= 0 disables the background cleanup goroutine so
// expired entries stay readable forever (Get does not expire lazily), and
// Shards must be a power of two within [1, 4096] — larger values would
// pre-allocate gigabytes of per-shard memory and multiply per-shard map
// overhead. Callers surface the returned error instead of panicking.
func validateConfig(c bigcache.Config) error {
	if c.LifeWindow < time.Second {
		return fmt.Errorf("LifeWindow must be >= 1s (bigcache time resolution), got %v", c.LifeWindow)
	}
	if c.CleanWindow <= 0 {
		return fmt.Errorf("CleanWindow must be > 0 to enable background eviction, got %v", c.CleanWindow)
	}
	if c.Shards <= 0 || c.Shards > 4096 || c.Shards&(c.Shards-1) != 0 {
		return fmt.Errorf("Shards must be a power of two in [1, 4096], got %d", c.Shards)
	}
	return nil
}

// rebuild (re)creates the underlying BigCache with the current config. Any
// previous instance is closed first so its cleanup goroutines do not leak.
// It returns the underlying library's construction error so callers can
// surface it explicitly instead of panicking.
func (s *bigcache_store) rebuild() error {
	if s.cache != nil {
		_ = s.cache.Close()
	}

	c, err := bigcache.New(context.Background(), s.config)
	if err != nil {
		return fmt.Errorf("bigcache: rebuild: %w", err)
	}

	s.cache = c
	return nil
}

// Initialize implements the optional cache.Options initialize hook: when the
// cache package is built with a global TTL (cache.WithTTL), the bigcache
// LifeWindow is linked to it so every entry shares that lifetime. It is only
// called by the cache package during construction; do not call it concurrently
// at runtime, as it may rebuild() and recreate the underlying BigCache.
func (s *bigcache_store) Initialize(o cache.Options) {
	if o.TTL > 0 {
		life := time.Duration(o.TTL) * time.Second
		if life != s.config.LifeWindow {
			s.config.LifeWindow = life
			// The Initialize hook has no error channel, but a rebuild error is
			// unreachable here: the config was validated at construction and
			// the new LifeWindow (TTL >= 1s) stays within bounds.
			_ = s.rebuild()
		}
	}
}

func (f *bigcache_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	data, err := f.cache.Get(key)
	if err != nil {
		if errors.Is(err, bigcache.ErrEntryNotFound) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("bigcache get key=%s: %w", key, err)
	}

	return data, true, nil
}

// maxShardSize returns the per-shard byte-queue capacity, or 0 when
// HardMaxCacheSize is 0 (unlimited). It mirrors the library's maxShardSize
// computation (bigcache/config.go): HardMaxCacheSize is in MB.
func (f *bigcache_store) maxShardSize() int {
	if f.config.HardMaxCacheSize <= 0 {
		return 0
	}
	return f.config.HardMaxCacheSize * 1024 * 1024 / f.config.Shards
}

// Put stores a key-value pair. bigcache does not support per-key TTL, so
// expireSeconds is intentionally ignored: every entry expires after the global
// LifeWindow regardless of the value passed here.
func (f *bigcache_store) Put(ctx context.Context, key string, v []byte, expireSeconds int) error {
	// Pre-check oversized values: when an entry does not fit a shard queue, the
	// library evicts oldest entries until it fits and, if the value alone is
	// larger than the whole queue, wipes the entire shard before failing.
	// Reject such writes up front with an explicit error.
	if max := f.maxShardSize(); max > 0 && len(v) > max {
		return fmt.Errorf("bigcache put key=%s: value size %d exceeds per-shard limit %d (HardMaxCacheSize=%dMB / Shards=%d)",
			key, len(v), max, f.config.HardMaxCacheSize, f.config.Shards)
	}

	if err := f.cache.Set(key, v); err != nil {
		return fmt.Errorf("bigcache put key=%s: %w", key, err)
	}

	return nil
}

// Delete removes keys and is idempotent: deleting a missing key (ErrEntryNotFound)
// is treated as success, and remaining keys are still removed. The first
// non-"not found" error is returned after processing all keys.
func (f *bigcache_store) Delete(ctx context.Context, key ...string) error {
	var firstErr error
	for _, k := range key {
		if err := f.cache.Delete(k); err != nil {
			if errors.Is(err, bigcache.ErrEntryNotFound) {
				continue
			}

			if firstErr == nil {
				firstErr = fmt.Errorf("bigcache delete key=%s: %w", k, err)
			}
		}
	}

	return firstErr
}

// Close releases the underlying bigcache resources (cleanup goroutines). It
// intentionally has no return value so it matches the cache package's Close
// detection (cache.cache.Close type-asserts interface{ Close() }); a
// Close() error signature would fail that assertion and the cleanup
// goroutines would never be stopped.
func (f *bigcache_store) Close() {
	f.closed = true
	_ = f.cache.Close()
}

func (*bigcache_store) Name() string { return "bigcache" }

func (*bigcache_store) IsRemote() bool { return false }
