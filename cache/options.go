package cache

import (
	"time"

	"github.com/charlienet/gadget/logger"
)

// Options represents the options for the cache.
type Options struct {
	localStore      Store
	remoteStore     Store
	listener        Listener
	serializer      Serializer
	metrics         Metrics
	Logger          logger.Logger
	TTL             int
	Name            string
	cleanupInterval time.Duration
	maxItems        int
	maxBytes        int64
	degradeThreshold   int
	degradeRecovery    time.Duration
	ttlJitter          time.Duration
	slidingWindow      time.Duration
	verifyEvery        int
	versionSyncInterval time.Duration
}

func (o Options) init() {
	o.initActual(o.localStore)
	o.initActual(o.remoteStore)
	o.initActual(o.listener)
}

func (o Options) initActual(v any) {
	if v == nil {
		return
	}

	if i, ok := v.(interface{ Initialize(Options) }); ok {
		i.Initialize(o)
	}
}

func (o *Options) WithStore(s Store) {
	if !s.IsRemote() {
		o.localStore = s
	} else {
		o.remoteStore = s
	}
}

func (o *Options) WithListener(l Listener) {
	o.listener = l
}

// Option manipulates the Options passed.
type Option func(o *Options)

func WithName(name string) Option {
	return func(o *Options) {
		o.Name = name
	}
}

func WithMemStore() Option {
	return func(o *Options) {
		o.WithStore(newMemStore())
	}
}

func WithStore(s Store) Option {
	return func(o *Options) {
		o.WithStore(s)
	}
}

func WithListener(lis Listener) Option {
	return func(o *Options) {
		o.listener = lis
	}
}

func WithSerializer(s Serializer) Option {
	return func(o *Options) {
		o.serializer = s
	}
}

func WithTTL(ttl int) Option {
	return func(o *Options) {
		o.TTL = ttl
	}
}

func WithLogger(l logger.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

func WithCleanupInterval(d time.Duration) Option {
	return func(o *Options) {
		o.cleanupInterval = d
	}
}

func WithMetrics(m Metrics) Option {
	return func(o *Options) {
		o.metrics = m
	}
}

func WithMaxItems(n int) Option {
	return func(o *Options) {
		o.maxItems = n
	}
}

func WithMaxBytes(n int64) Option {
	return func(o *Options) {
		o.maxBytes = n
	}
}

func WithDegradeThreshold(n int) Option {
	return func(o *Options) {
		o.degradeThreshold = n
	}
}

func WithDegradeRecoveryInterval(d time.Duration) Option {
	return func(o *Options) {
		o.degradeRecovery = d
	}
}

// WithTTLJitter adds a random jitter (0 ~ d) to every Put TTL to prevent
// multiple keys from expiring simultaneously (cache avalanche prevention).
func WithTTLJitter(d time.Duration) Option {
	return func(o *Options) {
		o.ttlJitter = d
	}
}

// WithSlidingWindow enables sliding expiration: a key's TTL is extended
// by its original TTL whenever it is accessed and the remaining TTL is
// less than the specified window. This prevents hot keys from expiring too early.
func WithSlidingWindow(d time.Duration) Option {
	return func(o *Options) {
		o.slidingWindow = d
	}
}

// WithVerifyEvery enables probabilistic local cache verification. After N local
// cache hits for a key, the next Get skips local and checks remote. If remote
// no longer has the data, the stale local entry is cleared immediately.
// 0 (default) disables verification.
func WithVerifyEvery(n int) Option {
	return func(o *Options) {
		o.verifyEvery = n
	}
}

// WithVersionSyncInterval enables a background goroutine that periodically
// checks local cache entries against the remote store and refreshes or
// evicts stale data. This provides pull-based cache coherence as a
// supplement to push-based PubSub invalidation.
//
// The goroutine iterates through local keys in batches (100 per cycle),
// comparing each key's value against the remote store. If remote has
// different data → update local. If remote no longer has the key → evict local.
// Skips the check when the cache is in degraded mode.
// Set to 0 to disable (default).
func WithVersionSyncInterval(d time.Duration) Option {
	return func(o *Options) {
		o.versionSyncInterval = d
	}
}
