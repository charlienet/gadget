package redis

import (
	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
)

type option func(*redis_store)

func New(rdb redis.Client, opts ...option) cache.Option {
	return func(o *cache.Options) {
		s := new(rdb, opts...)
		o.AddStore(s)
	}
}

func WithTTLFactor(factor int) option {
	return func(r *redis_store) {
		r.ttlFactor = factor
	}
}

func WithEmptyObject(empty string) option {
	return func(r *redis_store) {
		r.emptyObject = []byte(empty)
	}
}
