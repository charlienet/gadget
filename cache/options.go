package cache

import (
	"github.com/charlienet/gadget/logger"
)

// Options represents the options for the cache.
type Options struct {
	stores     []Store
	serializer Serializer
	logger     logger.Logger
}

func (o *Options) AddStore(s Store) {
	o.stores = append(o.stores, s)
}

// Option manipulates the Options passed.
type Option func(o *Options)

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
