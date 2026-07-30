package redis

import (
	"git.charlienet.top/go/gadget/cache"
	"git.charlienet.top/go/gadget/redis"
)

type option func(*redis_store)

func New(rdb redis.Client, opts ...option) cache.Option {
	return func(o *cache.Options) {
		s := new(rdb, opts...)
		o.WithStore(s)
	}
}

func WithTTLFactor(factor int) option {
	return func(r *redis_store) {
		r.ttlFactor = factor
	}
}
