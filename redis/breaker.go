package redis

import (
	"context"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// breakerState 熔断器状态。
type breakerState uint8

const (
	breakerClosed  breakerState = iota // 闭合：正常请求放行
	breakerOpen                        // 打开：快速失败（不实际连接）
	breakerHalfOpen                    // 半开：放行探测请求验证服务是否恢复
)

// 熔断器默认参数。
const (
	defaultBreakerThreshold = 3
	defaultBreakerCooldown  = time.Second
)

// CircuitBreaker 是自研熔断器状态机（三态）：
//
//   - Closed（闭合）：正常请求放行；连续失败达到阈值 → Open。
//   - Open（打开）：**快速失败**——直接返回最近一次错误，不实际连接
//     （避免每次请求都等待连接超时）；经过 cooldown 冷却期后 → HalfOpen。
//   - HalfOpen（半开）：放行一个探测请求（单飞，并发下同时只放行一个）；
//     探测成功 → Closed（自动恢复）；失败 → 回 Open（重置冷却）。
//
// 并发安全：状态与失败计数统一由互斥锁保护（状态切换与计数强相关，
// 用锁保持一致，不依赖原子操作）；HalfOpen 单飞通过锁内标记实现。
//
// 冷却计时采用惰性判断（Allow 时检查"上次失败时间 + cooldown <= now"），
// 无需定时器 goroutine，避免资源泄漏。
type CircuitBreaker struct {
	mu sync.Mutex

	state    breakerState
	failures int // 连续失败计数（仅连接类错误计数）

	threshold int           // 连续失败阈值（默认 3）
	cooldown  time.Duration // 冷却期（默认 1s，用户要求短：快速重连探测）

	lastErr      error     // 最近一次失败错误（Open 快速失败时返回）
	lastFailTime time.Time // 最近一次进入 Open 的时间（冷却计时起点）
	halfOpenTrial bool     // 半开探测标记：true 表示已有探测请求在途（单飞）
}

// newCircuitBreaker 创建熔断器（threshold/cooldown 非正时用默认值）。
func newCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = defaultBreakerThreshold
	}
	if cooldown <= 0 {
		cooldown = defaultBreakerCooldown
	}
	return &CircuitBreaker{
		state:     breakerClosed,
		threshold: threshold,
		cooldown:  cooldown,
	}
}

// Allow 判断请求是否允许执行：
//   - Closed → 允许（nil）
//   - Open → 冷却结束则转 HalfOpen 并放行首个探测请求；否则**快速失败**
//     （返回 lastErr，不实际连接）
//   - HalfOpen → 单飞：已有探测在途则拒绝（快速失败），否则放行探测
func (b *CircuitBreaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case breakerClosed:
		return nil

	case breakerOpen:
		if time.Since(b.lastFailTime) >= b.cooldown {
			// 冷却结束：转半开并放行首个探测请求（标记单飞）
			b.state = breakerHalfOpen
			b.halfOpenTrial = true
			return nil
		}
		return b.lastErr // 快速失败

	case breakerHalfOpen:
		if b.halfOpenTrial {
			return b.lastErr // 已有探测在途：拒绝（单飞）
		}
		b.halfOpenTrial = true
		return nil
	}
	return nil
}

// Success 记录成功：重置连续失败计数；半开探测成功 → 闭合（自动恢复）。
func (b *CircuitBreaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures = 0
	b.halfOpenTrial = false
	if b.state == breakerHalfOpen {
		b.state = breakerClosed
		b.lastErr = nil
	}
}

// Fail 记录连接类失败：连续失败达阈值 → Open（记录 lastErr 并启动冷却）；
// 半开探测失败 → 回 Open（重置冷却计时）。
func (b *CircuitBreaker) Fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == breakerHalfOpen {
		// 探测失败：回 Open，重置冷却
		b.state = breakerOpen
		b.lastFailTime = time.Now()
		b.lastErr = err
		b.halfOpenTrial = false
		return
	}

	b.failures++
	if b.failures >= b.threshold {
		b.state = breakerOpen
		b.lastFailTime = time.Now()
		b.lastErr = err
	}
}

// onResult 处理一次命令结果（hook 调用）：
//   - 成功 → Success（半开探测成功自动闭合）
//   - 连接类错误（IsUnavailable）→ Fail（计入熔断）
//   - 其他错误（命令级，如 WRONGTYPE）→ 不计入熔断；若处于半开探测
//     说明服务可达（连接正常），闭合恢复
func (b *CircuitBreaker) onResult(err error) {
	if err == nil {
		b.Success()
		return
	}
	if IsUnavailable(err) {
		b.Fail(err)
		return
	}

	// 非连接类错误：不计入熔断；半开探测时服务可达 → 闭合
	b.mu.Lock()
	if b.state == breakerHalfOpen {
		b.state = breakerClosed
		b.lastErr = nil
		b.halfOpenTrial = false
		b.failures = 0
	}
	b.mu.Unlock()
}

// breakerHook 是接入 go-redis hook 链的熔断 hook。
// 注册顺序关键：go-redis 的 hook 链"后注册的最外层"（withProcessHook 从
// slice 末尾向前包裹），因此熔断 hook 必须在 renameHook **之后**注册，
// 才能位于最外层：先熔断判断（Open 快速失败不执行命令），再前缀改写。
type breakerHook struct {
	breaker *CircuitBreaker
}

func (h *breakerHook) DialHook(next goredis.DialHook) goredis.DialHook {
	// 连接建立由 go-redis 连接池内部管理，直接透传
	return next
}

func (h *breakerHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if err := h.breaker.Allow(); err != nil {
			return err // 快速失败：不执行命令（含前缀改写）
		}
		err := next(ctx, cmd)
		h.breaker.onResult(err)
		return err
	}
}

func (h *breakerHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if err := h.breaker.Allow(); err != nil {
			return err
		}
		err := next(ctx, cmds)
		// 管道整体统计：最后一个错误判定（IsUnavailable 才计入熔断）
		h.breaker.onResult(err)
		return err
	}
}
