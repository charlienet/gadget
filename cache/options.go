package cache

import (
	"github.com/charlienet/gadget/logger"
)

// Options represents the options for the cache.
type Options struct {
	localStore  Store
	remoteStore Store
	serializer  Serializer
	logger      logger.Logger
	ttl         int
	// bloom      bloom.BloomFilter
}

func (o *Options) WithStore(s Store) {
	if !s.IsRemote() {
		o.localStore = s
	} else {
		o.remoteStore = s
	}
}

// Option manipulates the Options passed.
type Option func(o *Options)

func WithMemStore() Option {
	return func(o *Options) {
		o.WithStore(NewStore())
	}
}

func WithStore(s Store) Option {
	return func(o *Options) {
		o.WithStore(s)
	}
}

func WithLogger(l logger.Logger) Option {
	return func(o *Options) {
		o.logger = l
	}
}

func WithSerializer(s Serializer) Option {
	return func(o *Options) {
		o.serializer = s
	}
}

func WithTTL(ttl int) Option {
	return func(o *Options) {
		o.ttl = ttl
	}
}
