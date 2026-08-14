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
type RateLimiter struct {
	client    *redisClient
	name      string // 命名空间（空字符串表示不隔离，key 直通）
	separator string // 名称与 key 的分隔符（捕获自 client 前缀分隔符，默认 ":"）
}

// RateResult contains the result of a rate limit check.
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
func (rdb *redisClient) NewRateLimiter(name string) *RateLimiter {
	sep := rdb.prefix.separator
	if sep == "" {
		sep = defaultSeparator
	}

	return &RateLimiter{
		client:    rdb,
		name:      name,
		separator: sep,
	}
}

// limitKey 组合名称与业务 key：name 为空直通原 key，非空加命名空间前缀。
func (rl *RateLimiter) limitKey(key string) string {
	if rl.name == "" {
		return key
	}
	return rl.name + rl.separator + key
}

// allow 执行 GCRA 脚本并映射结果：rate 为窗口内允许次数，period 为窗口时长。
// 最终 Redis key = "rate:" + [name:]key，与旧版 redis_rate 的 key 结构一致。
func (rl *RateLimiter) allow(ctx context.Context, key string, rate int, period time.Duration) (*RateResult, error) {
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

// Allow checks if a request identified by key is allowed at the given rate
// (operations per second). Returns the result including remaining quota.
func (rl *RateLimiter) Allow(ctx context.Context, key string, ratePerSec int) (*RateResult, error) {
	return rl.allow(ctx, key, ratePerSec, time.Second)
}

// AllowN checks if a request identified by key is allowed at the given rate
// with a custom period. For example, AllowN(ctx, "api:1", 100, time.Minute)
// allows 100 operations per minute.
func (rl *RateLimiter) AllowN(ctx context.Context, key string, n int, per time.Duration) (*RateResult, error) {
	return rl.allow(ctx, key, n, per)
}

// allowAtMost 执行"尽力而为"的 GCRA 检查：请求消耗 cost 个配额，配额不足时
// 消耗剩余配额而非整批拒绝（"能发多少发多少"），适用批量请求场景。
func (rl *RateLimiter) allowAtMost(ctx context.Context, key string, rate int, period time.Duration, cost int) (*RateResult, error) {
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

// AllowAtMost 请求消耗 ratePerSec 配额的部分配额：配额充足时消耗 cost 全部放行
// （Consumed=cost）；不足时消耗剩余配额部分放行（Consumed=剩余量）；无剩余配额
// 时拒绝（Allowed=false、RetryAfter>0）。rate 语义同 Allow（每秒速率）。
func (rl *RateLimiter) AllowAtMost(ctx context.Context, key string, ratePerSec int, cost int) (*RateResult, error) {
	return rl.allowAtMost(ctx, key, ratePerSec, time.Second, cost)
}

// AllowAtMostN 同 AllowAtMost，rate 语义同 AllowN（per 窗口内 n 个）。
func (rl *RateLimiter) AllowAtMostN(ctx context.Context, key string, n int, per time.Duration, cost int) (*RateResult, error) {
	return rl.allowAtMost(ctx, key, n, per, cost)
}

// Reset 重置限流状态（删除状态 key），配额恢复满额。
// 适用于运维解除限流、限流配置变更后清空历史状态的场景。
func (rl *RateLimiter) Reset(ctx context.Context, key string) error {
	return rl.client.Del(ctx, "rate:"+rl.limitKey(key)).Err()
}

// Wait 阻塞直到配额放行或 ctx 取消/超时（限速语义：等待而非拒绝）。
// 被拒时等待 RetryAfter 后重试；RetryAfter 为 0 时最小等待 1ms 防忙循环。
func (rl *RateLimiter) Wait(ctx context.Context, key string, ratePerSec int) error {
	return waitLoop(ctx, func(ctx context.Context) (*RateResult, error) {
		return rl.Allow(ctx, key, ratePerSec)
	})
}

// WaitN 阻塞直到配额放行或 ctx 取消/超时（AllowN 的等待版）。
func (rl *RateLimiter) WaitN(ctx context.Context, key string, n int, per time.Duration) error {
	return waitLoop(ctx, func(ctx context.Context) (*RateResult, error) {
		return rl.AllowN(ctx, key, n, per)
	})
}

// minWaitInterval 是 RetryAfter 为 0 时的最小等待时长，防止忙循环。
const minWaitInterval = time.Millisecond

// waitLoop 通用的阻塞等待循环：反复调用 allow，被拒时等待 RetryAfter
// （不足时取最小等待）后重试；放行返回 nil，ctx 取消/超时返回 ctx.Err()。
// 每次调用独立，无需额外锁（并发安全由 Redis 侧原子性保证）。
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
