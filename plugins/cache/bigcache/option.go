package bigcache

import (
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/charlienet/gadget/cache"
)

// ConfigFunc customizes the bigcache.Config used by NewBigCache. It lets
// callers override the safe defaults (256MB hard cap, 128 shards, 1-minute
// LifeWindow) on a per-field basis.
type ConfigFunc func(*bigcache.Config)

// WithHardMaxCacheSize sets the hard memory limit in MB (0 = unlimited).
// When reached, the oldest entries are overridden for new ones, protecting the
// application from OOM.
func WithHardMaxCacheSize(mb int) ConfigFunc {
	return func(c *bigcache.Config) { c.HardMaxCacheSize = mb }
}

// WithShards sets the number of cache shards. It must be a power of two, and
// it also affects the per-shard memory pre-allocation (see MaxEntriesInWindow).
func WithShards(n int) ConfigFunc {
	return func(c *bigcache.Config) { c.Shards = n }
}

// WithMaxEntrySize sets the maximum entry size in bytes (used to size the
// pre-allocated per-shard queues).
func WithMaxEntrySize(n int) ConfigFunc {
	return func(c *bigcache.Config) { c.MaxEntrySize = n }
}

// WithLifeWindow sets the global entry lifetime. bigcache does not support
// per-key TTL: every entry expires once LifeWindow elapses, regardless of the
// expireSecond passed to Put. Note that the underlying library only evicts
// expired entries during background cleanups, so keep CleanWindow <= LifeWindow
// to guarantee timely eviction.
func WithLifeWindow(d time.Duration) ConfigFunc {
	return func(c *bigcache.Config) { c.LifeWindow = d }
}

// WithCleanWindow sets how often the background cleanup removes expired
// entries. Must be > 0 to enable eviction of entries past LifeWindow.
func WithCleanWindow(d time.Duration) ConfigFunc {
	return func(c *bigcache.Config) { c.CleanWindow = d }
}

// WithMaxEntriesInWindow sets the expected number of entries per life window.
// Together with MaxEntrySize it determines the per-shard pre-allocation
// (initialShardSize x MaxEntrySize bytes per shard); lower values reduce
// startup memory.
func WithMaxEntriesInWindow(n int) ConfigFunc {
	return func(c *bigcache.Config) { c.MaxEntriesInWindow = n }
}

// New returns a cache.Option that installs a bigcache-backed local store with
// safe defaults, customizable via config funcs. It returns an error when the
// combined configuration fails validation (see NewBigCache for the rules).
func New(configs ...ConfigFunc) (cache.Option, error) {
	s, err := NewBigCache(configs...)
	if err != nil {
		return nil, err
	}

	return func(o *cache.Options) {
		o.WithStore(s)
	}, nil
}
