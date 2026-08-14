package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// LeakyBucketOption 配置漏桶。
type LeakyBucketOption func(*leakyBucketConfig)

type leakyBucketConfig struct {
	failPolicyConfig
	burst int // 桶容量（允许的最大排队请求数），默认等于速率（rate）
}

// LeakyBucketConfig 是 LeakyBucket 的配置类型别名，供 WithFailPolicy 泛型参数使用。
type LeakyBucketConfig = leakyBucketConfig

func defaultLeakyBucketConfig() leakyBucketConfig { return leakyBucketConfig{} }

// WithBurst 设置漏桶容量（允许的最大排队请求数），默认等于速率。
// burst=1 时不允许任何排队：同一时刻仅一个请求放行，其余拒绝。
func WithBurst(n int) LeakyBucketOption {
	return func(c *leakyBucketConfig) {
		if n > 0 {
			c.burst = n
		}
	}
}

// LeakyBucket 是基于 Redis 的漏桶限流器（nextAvailableTime 法）。
// 与令牌桶（RateLimiter）互补：
//   - 漏桶：输出速率严格恒定、拒绝突发——请求按固定间隔（interval）放行，
//     超出桶容量（burst）的排队请求被拒绝（或由 Wait 阻塞等待）。
//   - 令牌桶：允许突发，按平均速率补令牌。
//
// 状态存储：单个 key 存"下一次可用时刻"（UnixMilli 整数），Lua 单次往返。
// key 组合与 NewRateLimiter 一致：name 非空时 = name + separator + key，
// 空名称直通（不隔离）；最终 Redis key = 统一前缀 + [name:]key。
//
// 使用示例：
//
//	lb := rdb.NewLeakyBucket("sms", redis.WithBurst(5))
//	res, err := lb.Allow(ctx, "user:1", 10) // 每秒恒定输出 10 个
//	if !res.Allowed {
//		// 排队超过桶容量：res.RetryAfter 为建议等待时长
//	}
type LeakyBucket struct {
	client    *redisClient
	name      string
	separator string
	burst     int
	policy    FailPolicy // 失效兜底策略（默认 FailOpen）
}

// NewLeakyBucket 创建漏桶限流器（挂 *redisClient）。
// name 语义与 NewRateLimiter 一致：非空按名称隔离限流 key 空间，
// 空名称不隔离。
func (rdb *redisClient) NewLeakyBucket(name string, opts ...LeakyBucketOption) *LeakyBucket {
	cfg := defaultLeakyBucketConfig()
	cfg.policy = FailOpen // 漏桶默认 FailOpen：保护性能力，宁可多放
	for _, o := range opts {
		o(&cfg)
	}

	sep := rdb.prefix.separator
	if sep == "" {
		sep = defaultSeparator
	}

	return &LeakyBucket{
		client:    rdb,
		name:      name,
		separator: sep,
		burst:     cfg.burst,
		policy:    cfg.policy,
	}
}

// limitKey 组合名称与业务 key：name 为空直通原 key，非空加命名空间前缀。
func (lb *LeakyBucket) limitKey(key string) string {
	if lb.name == "" {
		return key
	}
	return lb.name + lb.separator + key
}

// leakyBucketScript 漏桶核心（nextAvailableTime 法）：
//   - next = 状态中"下一次可用时刻"；cur = max(now, next) 为本次请求实际执行时刻
//   - 排队量 cur-now 超过桶容量（burst_window_ms = burst × interval）→ 拒绝
//     （返回 {0, 排队量}，排队量即建议等待时长）
//   - 否则放行并推进 next = cur + interval（返回 {1, 0}）
//
// 状态 key 带 PX 过期（防长期不使用的 key 堆积），过期时长在 Go 侧计算。
var leakyBucketScript = goredis.NewScript(`
local next = tonumber(redis.call('GET', KEYS[1]) or '0')
local cur = math.max(tonumber(ARGV[1]), next)
if cur - tonumber(ARGV[1]) >= tonumber(ARGV[3]) then
	return {0, cur - tonumber(ARGV[1])}
end
redis.call('SET', KEYS[1], cur + tonumber(ARGV[2]), 'PX', ARGV[4])
return {1, 0}
`)

// allowRaw 漏桶核心（不做失效兜底，返回原始错误，供 Wait 使用）。
// rate = n/per，interval = per/n。返回值：Allowed=放行；RetryAfter=被拒时
// 的建议等待时长（毫秒）。
func (lb *LeakyBucket) allowRaw(ctx context.Context, key string, n int, per time.Duration) (*RateResult, error) {
	if n <= 0 {
		return nil, fmt.Errorf("redis: 漏桶速率必须为正数，got %d", n)
	}

	// interval = per/n（毫秒），至少 1ms（极高速率时避免 interval=0 无进展）
	intervalMs := max(per.Milliseconds()/int64(n), 1)

	burst := lb.burst
	if burst <= 0 {
		burst = n // 默认桶容量 = 速率
	}
	burstWindowMs := int64(burst) * intervalMs
	// 状态 key 过期时间：覆盖桶容量窗口 + 一个间隔，防止长期不用的 key 堆积
	expireMs := burstWindowMs*2 + intervalMs

	nowMs := time.Now().UnixMilli()
	res, err := leakyBucketScript.Run(ctx, lb.client, []string{lb.limitKey(key)},
		nowMs, intervalMs, burstWindowMs, expireMs).Int64Slice()
	if err != nil {
		return nil, err
	}
	if len(res) < 2 {
		return nil, fmt.Errorf("redis: 漏桶脚本返回异常结果 %v", res)
	}

	return &RateResult{
		Allowed:    res[0] == 1,
		Remaining:  0, // 简化：漏桶语义下"剩余容量"意义不大，见类型注释
		RetryAfter: time.Duration(res[1]) * time.Millisecond,
	}, nil
}

// fallbackRateResult 按策略返回漏桶兜底值 + 哨兵错误：FailOpen → 放行；
// FailClosed → 拒绝。错误为 ErrRedisUnavailable 包装（errors.Is 可感知）。
func (lb *LeakyBucket) fallbackRateResult(err error) (*RateResult, error) {
	if lb.policy == FailOpen {
		return &RateResult{Allowed: true}, fallbackErr(err)
	}
	return &RateResult{Allowed: false}, fallbackErr(err)
}

// allow 执行漏桶检查并处理失效兜底（Allow/AllowN 使用）。
func (lb *LeakyBucket) allow(ctx context.Context, key string, n int, per time.Duration) (*RateResult, error) {
	res, err := lb.allowRaw(ctx, key, n, per)
	if err != nil && isUnavailable(err) {
		return lb.fallbackRateResult(err)
	}
	return res, err
}

// Allow 检查请求是否放行：ratePerSec 为每秒输出速率（burst 默认 = ratePerSec）。
// 被拒（Allowed=false）时 RetryAfter 为建议等待时长，可用 Wait 阻塞等待。
func (lb *LeakyBucket) Allow(ctx context.Context, key string, ratePerSec int) (*RateResult, error) {
	return lb.allow(ctx, key, ratePerSec, time.Second)
}

// AllowN 检查请求是否放行：per 时间窗口内允许 n 个请求
// （interval = per/n，burst 默认 = n）。
func (lb *LeakyBucket) AllowN(ctx context.Context, key string, n int, per time.Duration) (*RateResult, error) {
	return lb.allow(ctx, key, n, per)
}

// Wait 阻塞直到配额放行或 ctx 取消/超时（限速语义：等待而非拒绝）。
// 被拒时等待 RetryAfter 后重试；RetryAfter 为 0 时最小等待 1ms 防忙循环。
// Redis 服务失效时按兜底策略：FailOpen → 直接放行返回 nil；FailClosed →
// 返回错误（不能吞错死循环等待）。
func (lb *LeakyBucket) Wait(ctx context.Context, key string, ratePerSec int) error {
	return waitLoop(ctx, func(ctx context.Context) (*RateResult, error) {
		res, err := lb.allowRaw(ctx, key, ratePerSec, time.Second)
		if err != nil && isUnavailable(err) {
			if lb.policy == FailOpen {
				// FailOpen：放行但返回哨兵错误（应用层感知"放行是兜底的"）
				return &RateResult{Allowed: true}, fallbackErr(err)
			}
			return nil, fallbackErr(err) // FailClosed：返回错误（不死循环）
		}
		return res, err
	})
}

// WaitN 阻塞直到配额放行或 ctx 取消/超时（AllowN 的等待版）。
func (lb *LeakyBucket) WaitN(ctx context.Context, key string, n int, per time.Duration) error {
	return waitLoop(ctx, func(ctx context.Context) (*RateResult, error) {
		res, err := lb.allowRaw(ctx, key, n, per)
		if err != nil && isUnavailable(err) {
			if lb.policy == FailOpen {
				return &RateResult{Allowed: true}, fallbackErr(err)
			}
			return nil, fallbackErr(err)
		}
		return res, err
	})
}
