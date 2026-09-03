package redis

import (
	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/retry"
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

// WithRetry 开启操作级重试（opt-in，默认关闭）。
// 无参调用启用插件默认策略：3 次尝试 + EqualJitter(Exponential(50ms,×2,封顶1s))
// + 仅 redis.IsUnavailable 类错误可重试（miss/WRONGTYPE/ctx 取消不重试）。
// 传入的 retry.Option 在默认之后应用，可覆盖 MaxAttempts/Backoff/Retryable。
// 注意：不要传入与 retry 包共享的 Backoff 实例（非并发安全）；插件默认退避在每次调用时新建。
// 注意：开启后若 ctx 在退避期间到期，返回 ctx.Err() 而非最后一次网络错误；建议在带 deadline 的 ctx 下使用。
func WithRetry(opts ...retry.Option) option {
	return func(r *redis_store) {
		r.retryOn = true
		r.retryOpts = append([]retry.Option(nil), opts...)
	}
}
