package redis

import (
	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
)

type option func(*redis_store)

func New(rdb redis.Client, opts ...option) cache.Option {
	return func(o *cache.Options) {
		s := new(rdb, opts...)
		o.WithStore(s)
	}
}

// WithTTLFactor 配置 TTL 防雪崩随机偏移（默认 30，开启）。
//   - factor <= 1：关闭随机偏移，TTL 所见即所得（Put 传多少秒写多少秒）；
//   - factor > 1：每次 Put 在 expireSeconds 基础上叠加 [1, factor-1] 秒随机值，
//     使同批 key 的过期时间分散（默认 30 即叠加 [1,29] 秒）。
func WithTTLFactor(factor int) option {
	return func(r *redis_store) {
		r.ttlFactor = factor
	}
}
