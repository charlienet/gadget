package redis

import (
	"context"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// tokenBucketScript 是自研 GCRA（Generic Cell Rate Algorithm）令牌桶 Lua 脚本，
// 语义与 go-redis/redis_rate 的 allowN 完全一致（cost 固定 1），用于替换该
// 第三方依赖（MIT）。算法要点：
//
//   - 状态：单 key 存 TAT（Theoretical Arrival Time，理论到达时刻），
//     浮点秒，以 2017-01-01 为基准偏移避免 64 位浮点精度问题。
//   - now 取自 redis TIME（秒 + 微秒）；tat = max(tat, now)（时钟回拨保护）。
//   - emission_interval = period / rate；burst_offset = interval × burst。
//   - 放行条件：now ≥ new_tat - burst_offset（即剩余配额 diff/interval ≥ 0）；
//     放行后推进 tat = tat + interval（cost=1）。
//   - 拒绝：返回 retry_after = -diff（需等待时长）。
//   - 放行时 SET 带 EX（ceil(reset_after)），防长期不用的 key 堆积。
//
// 返回 {allowed, remaining, retry_after(秒), reset_after(秒)}，
// retry_after/reset_after 以字符串返回（浮点秒需完整精度，RESP integer 会截断）。
var tokenBucketScript = goredis.NewScript(`
-- this script has side-effects, so it requires replicate commands mode
redis.replicate_commands()

local rate_limit_key = KEYS[1]
local burst = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local period = tonumber(ARGV[3])

local emission_interval = period / rate
local burst_offset = emission_interval * burst

local jan_1_2017 = 1483228800
local now = redis.call('TIME')
now = (now[1] - jan_1_2017) + (now[2] / 1000000)

local tat = tonumber(redis.call('GET', rate_limit_key) or '0')
tat = math.max(tat, now)

local new_tat = tat + emission_interval
local allow_at = new_tat - burst_offset
local diff = now - allow_at
local remaining = diff / emission_interval

if remaining < 0 then
	local reset_after = tat - now
	local retry_after = diff * -1
	return {0, 0, tostring(retry_after), tostring(reset_after)}
end

local reset_after = new_tat - now
if reset_after > 0 then
	redis.call('SET', rate_limit_key, new_tat, 'EX', math.ceil(reset_after))
end
return {1, remaining, tostring(-1), tostring(reset_after)}
`)

// tokenBucketAtMostScript 是 AllowAtMost 的 GCRA 脚本（"尽力而为"语义）：
//   - remaining < 1 → 拒绝（retry_after = emission_interval - diff）
//   - remaining < cost → cost 裁剪为 remaining、remaining=0、放行
//   - 否则 → remaining -= cost、放行
//
// 返回 {实际消耗 cost, remaining, retry_after(秒), reset_after(秒)}，
// 其余（TIME 取时、TAT、EX 过期）与 tokenBucketScript 一致。
var tokenBucketAtMostScript = goredis.NewScript(`
-- this script has side-effects, so it requires replicate commands mode
redis.replicate_commands()

local rate_limit_key = KEYS[1]
local burst = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local period = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])

local emission_interval = period / rate
local burst_offset = emission_interval * burst

local jan_1_2017 = 1483228800
local now = redis.call('TIME')
now = (now[1] - jan_1_2017) + (now[2] / 1000000)

local tat = tonumber(redis.call('GET', rate_limit_key) or '0')
tat = math.max(tat, now)

local diff = now - (tat - burst_offset)
local remaining = diff / emission_interval

if remaining < 1 then
	local reset_after = tat - now
	local retry_after = emission_interval - diff
	return {0, 0, tostring(retry_after), tostring(reset_after)}
end

if remaining < cost then
	cost = remaining
	remaining = 0
else
	remaining = remaining - cost
end

local new_tat = tat + emission_interval * cost

local reset_after = new_tat - now
if reset_after > 0 then
	redis.call('SET', rate_limit_key, new_tat, 'EX', math.ceil(reset_after))
end
return {cost, remaining, tostring(-1), tostring(reset_after)}
`)

// RateLimiter 是自研 GCRA 令牌桶限流器（不依赖第三方限流库）。
// 语义：允许突发（burst = 速率），按平均速率补令牌，与漏桶（LeakyBucket，
// 恒定输出速率）互补。支持按名称隔离限流 key 空间：同一 client 创建的多个
// 限流器（不同 name）互不干扰；同一 name 内相同业务 key 共享配额。
//
// 失效兜底策略默认 FailOpen（保护性能力，服务不可用时宁可多放也不阻塞业务）；
// 可用 WithFailPolicy 显式改为 FailClosed。
//
// Deprecated: 请改用独立模块 github.com/charlienet/gadget/ratelimit（限流器抽象），
// 配合后端插件 github.com/charlienet/gadget/plugins/ratelimit/redis（GCRA 批发脚本）。
// 迁移时的语义差异：
//
//   - 速率/突发：旧接口每次调用传参（ratePerSec/per），新模块经
//     ratelimit.WithRate/WithBurst 在 Limiter 实例级固定；不同速率组合需建多个 Limiter。
//   - 结果表达：旧 RateResult{Allowed,Remaining,Consumed,RetryAfter} 用结构体字段表达
//     超限；新模块用 (bool, error)，判定 errors.Is(err, ratelimit.ErrExceeded)，经
//     errors.As(*ratelimit.ExceededError) 取 RetryAfter，Remaining 不回传。
//   - 尽力扣减：AllowAtMost*（能扣多少扣多少）对应新模块 Backend 层的
//     ratelimit.GrantBestEffort 授予模式。
//   - Reset：新模块无逐 key 重置对应，靠闲置回收（WithIdleRetention）或后端 TTL 自然过期。
//   - 精确逐次判定：可用 ratelimit.WithoutLocalLease() 关闭本地租约。
//
// 本类型是 Client 导出接口成员，为兼容保留，不再演进。
type RateLimiter struct {
	client    *redisClient
	name      string // 命名空间（空字符串表示不隔离，key 直通）
	separator string // 名称与 key 的分隔符（捕获自 client 前缀分隔符，默认 ":"）
	policy    FailPolicy
}

// RateLimiterOption 配置限流器（WithFailPolicy 泛型参数使用
// *RateLimiter 类型，见 failover.go）。
//
// Deprecated: 随 RateLimiter 一并弃用，请改用 ratelimit 模块的 Option。
type RateLimiterOption func(*RateLimiter)

// setPolicy 实现 failPolicySetter（供 WithFailPolicy 泛型约束使用）。
func (rl *RateLimiter) setPolicy(p FailPolicy) {
	rl.policy = p
}

// RateResult contains the result of a rate limit check.
// 令牌桶（RateLimiter）与漏桶（LeakyBucket）两族共享此结果类型。
//
// Deprecated: 随旧限流族一并弃用，请改用 ratelimit 模块的 (bool, error) 返回，
// 超限细节经 errors.As(*ratelimit.ExceededError) 取出（Remaining 不再回传）。
type RateResult struct {
	// Allowed indicates whether the request is allowed.
	// AllowAtMost 场景下部分放行（消耗了部分配额）也算 true。
	Allowed bool

	// Remaining is the remaining quota in the current window.
	Remaining int

	// RetryAfter is the duration to wait before retrying (when not allowed).
	RetryAfter time.Duration

	// Consumed 是本次请求实际消耗的配额数：
	// Allow/AllowN 固定为 1；AllowAtMost/AllowAtMostN 在配额不足时返回
	// 裁剪后的实际消耗值（"能发多少发多少"）。
	Consumed int
}

// NewRateLimiter 创建限流器。name 为命名空间：
//   - name 非空：限流 key 组合为 name + separator + key，不同 name 的限流器
//     互不干扰（如 NewRateLimiter("login") 与 NewRateLimiter("pay") 隔离）。
//   - name 为空：key 直通，不隔离（行为与旧版一致）。
//
// 最终 Redis key = 客户端前缀 + "rate:" + [name:]key（前缀由 hook 统一添加）。
// 使用示例：
//
//	login := rdb.NewRateLimiter("login")
//	pay := rdb.NewRateLimiter("pay")
//	// login 与 pay 对相同业务 key 互不影响（key 空间按名称隔离）
//	login.Allow(ctx, "user:1", 5)
//	pay.Allow(ctx, "user:1", 10)
//
// Deprecated: 请改用 ratelimit.New（配合 plugins/ratelimit/redis 后端），差异见 RateLimiter。
func (rdb *redisClient) NewRateLimiter(name string, opts ...RateLimiterOption) *RateLimiter {
	sep := rdb.prefix.separator
	if sep == "" {
		sep = defaultSeparator
	}

	rl := &RateLimiter{
		client:    rdb,
		name:      name,
		separator: sep,
		policy:    FailOpen, // 限流默认 FailOpen：保护性能力，宁可多放
	}
	for _, o := range opts {
		o(rl)
	}
	return rl
}

// limitKey 组合名称与业务 key：name 为空直通原 key，非空加命名空间前缀。
func (rl *RateLimiter) limitKey(key string) string {
	if rl.name == "" {
		return key
	}
	return rl.name + rl.separator + key
}

// allowRaw 执行 GCRA 脚本并映射结果，**不做失效兜底**（返回原始错误，
// 供 Wait/WaitN 使用——避免 FailClosed 兜底吞错后 Wait 死循环）。
func (rl *RateLimiter) allowRaw(ctx context.Context, key string, rate int, period time.Duration) (*RateResult, error) {
	if rate <= 0 {
		return nil, fmt.Errorf("redis: 限流速率必须为正数，got %d", rate)
	}
	if period <= 0 {
		return nil, fmt.Errorf("redis: 限流窗口必须为正数，got %v", period)
	}

	v, err := tokenBucketScript.Run(ctx, rl.client,
		[]string{"rate:" + rl.limitKey(key)},
		rate, rate, period.Seconds()).Result()
	if err != nil {
		return nil, err
	}

	values, ok := v.([]interface{})
	if !ok || len(values) < 4 {
		return nil, fmt.Errorf("redis: 令牌桶脚本返回异常结果 %v", v)
	}

	allowed, _ := values[0].(int64)
	remaining, _ := values[1].(int64)
	retryAfter, err := strconv.ParseFloat(values[2].(string), 64)
	if err != nil {
		return nil, fmt.Errorf("redis: 解析 retry_after 失败: %w", err)
	}

	res := &RateResult{
		Allowed:   allowed == 1,
		Remaining: int(remaining),
		Consumed:  1, // Allow/AllowN 固定消耗 1 个配额
	}
	// 放行时 retry_after = -1（占位），仅被拒时为正
	if retryAfter > 0 {
		res.RetryAfter = time.Duration(retryAfter * float64(time.Second))
	}
	return res, nil
}

// fallbackRateResult 按策略返回限流兜底值 + 哨兵错误：FailOpen → 放行
// （Allowed=true, Consumed=1）；FailClosed → 拒绝（Allowed=false）。
// 错误为 ErrRedisUnavailable 包装（errors.Is 可感知兜底发生）。
func (rl *RateLimiter) fallbackRateResult(err error) (*RateResult, error) {
	if rl.policy == FailOpen {
		return &RateResult{Allowed: true, Consumed: 1}, fallbackErr(err)
	}
	return &RateResult{Allowed: false}, fallbackErr(err)
}

// allow 执行 GCRA 检查并处理失效兜底（Allow/AllowN/AllowAtMost 使用）。
func (rl *RateLimiter) allow(ctx context.Context, key string, rate int, period time.Duration) (*RateResult, error) {
	res, err := rl.allowRaw(ctx, key, rate, period)
	if err != nil && IsUnavailable(err) {
		return rl.fallbackRateResult(err)
	}
	return res, err
}

// Allow checks if a request identified by key is allowed at the given rate
// (operations per second). Returns the result including remaining quota.
//
// Deprecated: 请改用 ratelimit.Limiter.Allow（速率经 WithRate 固定），差异见 RateLimiter。
func (rl *RateLimiter) Allow(ctx context.Context, key string, ratePerSec int) (*RateResult, error) {
	return rl.allow(ctx, key, ratePerSec, time.Second)
}

// AllowN checks if a request identified by key is allowed at the given rate
// with a custom period. For example, AllowN(ctx, "api:1", 100, time.Minute)
// allows 100 operations per minute.
//
// Deprecated: 请改用 ratelimit.Limiter.Allow（速率经 WithRate 固定），差异见 RateLimiter。
func (rl *RateLimiter) AllowN(ctx context.Context, key string, n int, per time.Duration) (*RateResult, error) {
	return rl.allow(ctx, key, n, per)
}

// allowAtMostRaw 执行"尽力而为"的 GCRA 检查（不做失效兜底，返回原始错误，
// 供 WaitAtMost 语义的一致性使用；当前 Wait 系列只用于 Allow/AllowN）。
func (rl *RateLimiter) allowAtMostRaw(ctx context.Context, key string, rate int, period time.Duration, cost int) (*RateResult, error) {
	if rate <= 0 {
		return nil, fmt.Errorf("redis: 限流速率必须为正数，got %d", rate)
	}
	if period <= 0 {
		return nil, fmt.Errorf("redis: 限流窗口必须为正数，got %v", period)
	}
	if cost <= 0 {
		return nil, fmt.Errorf("redis: 消耗配额必须为正数，got %d", cost)
	}

	v, err := tokenBucketAtMostScript.Run(ctx, rl.client,
		[]string{"rate:" + rl.limitKey(key)},
		rate, rate, period.Seconds(), cost).Result()
	if err != nil {
		return nil, err
	}

	values, ok := v.([]interface{})
	if !ok || len(values) < 4 {
		return nil, fmt.Errorf("redis: 令牌桶脚本返回异常结果 %v", v)
	}

	consumed, _ := values[0].(int64)
	remaining, _ := values[1].(int64)
	retryAfter, err := strconv.ParseFloat(values[2].(string), 64)
	if err != nil {
		return nil, fmt.Errorf("redis: 解析 retry_after 失败: %w", err)
	}

	res := &RateResult{
		Allowed:   consumed > 0, // 部分放行（消耗部分配额）也算放行
		Remaining: int(remaining),
		Consumed:  int(consumed),
	}
	if retryAfter > 0 {
		res.RetryAfter = time.Duration(retryAfter * float64(time.Second))
	}
	return res, nil
}

// allowAtMost 执行"尽力而为"的 GCRA 检查并处理失效兜底。
// FailOpen → (Allowed=true, Consumed=1) 放行；FailClosed → (Allowed=false)。
func (rl *RateLimiter) allowAtMost(ctx context.Context, key string, rate int, period time.Duration, cost int) (*RateResult, error) {
	res, err := rl.allowAtMostRaw(ctx, key, rate, period, cost)
	if err != nil && IsUnavailable(err) {
		return rl.fallbackRateResult(err)
	}
	return res, err
}

// AllowAtMost 请求消耗 ratePerSec 配额的部分配额：配额充足时消耗 cost 全部放行
// （Consumed=cost）；不足时消耗剩余配额部分放行（Consumed=剩余量）；无剩余配额
// 时拒绝（Allowed=false、RetryAfter>0）。rate 语义同 Allow（每秒速率）。
//
// Deprecated: 尽力扣减语义对应 ratelimit 的 GrantBestEffort 授予模式（Backend 层），差异见 RateLimiter。
func (rl *RateLimiter) AllowAtMost(ctx context.Context, key string, ratePerSec int, cost int) (*RateResult, error) {
	return rl.allowAtMost(ctx, key, ratePerSec, time.Second, cost)
}

// AllowAtMostN 同 AllowAtMost，rate 语义同 AllowN（per 窗口内 n 个）。
//
// Deprecated: 随 AllowAtMost 一并弃用，对应 ratelimit 的 GrantBestEffort，差异见 RateLimiter。
func (rl *RateLimiter) AllowAtMostN(ctx context.Context, key string, n int, per time.Duration, cost int) (*RateResult, error) {
	return rl.allowAtMost(ctx, key, n, per, cost)
}

// Reset 重置限流状态（删除状态 key），配额恢复满额。
// 适用于运维解除限流、限流配置变更后清空历史状态的场景。
//
// Deprecated: 新模块无逐 key 重置对应，靠闲置回收/后端 TTL 过期，差异见 RateLimiter。
func (rl *RateLimiter) Reset(ctx context.Context, key string) error {
	return rl.client.Del(ctx, "rate:"+rl.limitKey(key)).Err()
}

// Wait 阻塞直到配额放行或 ctx 取消/超时（限速语义：等待而非拒绝）。
// 被拒时等待 RetryAfter 后重试；RetryAfter 为 0 时最小等待 1ms 防忙循环。
// Redis 服务失效时按兜底策略：FailOpen → 直接放行返回 nil；FailClosed →
// 返回错误（不能吞错死循环等待）。
//
// Deprecated: 请改用 ratelimit.Limiter.Wait（速率经 WithRate 固定），差异见 RateLimiter。
func (rl *RateLimiter) Wait(ctx context.Context, key string, ratePerSec int) error {
	return waitLoop(ctx, func(ctx context.Context) (*RateResult, error) {
		res, err := rl.allowRaw(ctx, key, ratePerSec, time.Second)
		if err != nil && IsUnavailable(err) {
			if rl.policy == FailOpen {
				// FailOpen：放行但返回哨兵错误（应用层感知"放行是兜底的"）
				return &RateResult{Allowed: true}, fallbackErr(err)
			}
			return nil, fallbackErr(err) // FailClosed：返回错误（不死循环）
		}
		return res, err
	})
}

// WaitN 阻塞直到配额放行或 ctx 取消/超时（AllowN 的等待版）。
//
// Deprecated: 请改用 ratelimit.Limiter.Wait（速率经 WithRate 固定），差异见 RateLimiter。
func (rl *RateLimiter) WaitN(ctx context.Context, key string, n int, per time.Duration) error {
	return waitLoop(ctx, func(ctx context.Context) (*RateResult, error) {
		res, err := rl.allowRaw(ctx, key, n, per)
		if err != nil && IsUnavailable(err) {
			if rl.policy == FailOpen {
				return &RateResult{Allowed: true}, fallbackErr(err)
			}
			return nil, fallbackErr(err)
		}
		return res, err
	})
}

// waitLoop 通用的阻塞等待循环：反复调用 allow，被拒时等待 RetryAfter
// （不足时取最小等待）后重试；放行返回 nil，ctx 取消/超时返回 ctx.Err()。
// 每次调用独立，无需额外锁（并发安全由 Redis 侧原子性保证）。
// 供 RateLimiter 与 LeakyBucket 的 Wait/WaitN 共用。
func waitLoop(ctx context.Context, allow func(context.Context) (*RateResult, error)) error {
	for {
		res, err := allow(ctx)
		if err != nil {
			return err
		}
		if res.Allowed {
			return nil
		}

		wait := res.RetryAfter
		if wait <= 0 {
			wait = minWaitInterval
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// minWaitInterval 是 RetryAfter 为 0 时的最小等待时长，防止忙循环。
const minWaitInterval = time.Millisecond
