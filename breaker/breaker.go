// Package breaker 提供通用三态熔断器（Closed/Open/HalfOpen），
// 纯标准库实现，零第三方依赖。
//
// 两种使用形态：
//   - Execute：一站式包装（推荐，消除 TwoStep 的半开探测泄漏风险）；
//   - TwoStep：Allow 与 Success/Fail/Report 分离（hook/中间件场景——
//     调用点与结果回调点不在同一函数栈时使用）。
package breaker

import (
	"sync"
	"time"
)

// State 是熔断器状态。
type State uint8

const (
	// Closed 正常放行；连续失败计数达阈值 → Open。
	Closed State = iota
	// Open 快速失败：Allow 原样返回 lastErr（不实际执行调用）；
	// 冷却期结束 → HalfOpen。
	Open
	// HalfOpen 单飞探测：放行一个探测请求；探测成功 → Closed，
	// 探测失败 → 回 Open（重置冷却）。
	HalfOpen
)

// String 返回状态的小写名称："closed" / "open" / "half-open"。
func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Classifier 判定 err 是否计为熔断失败（true = 计入）。
// 极性正向（"计入失败"），与 retry.WithRetryable 风格一致。
// 默认：所有非 nil 错误计为失败；接入具体客户端时注入精确判定
// （如 gadget/redis 的 IsUnavailable：仅连接/服务类故障计入）。
type Classifier func(err error) bool

// Breaker 是三态熔断器状态机：
//
//   - Closed（闭合）：正常请求放行；连续失败达到阈值 → Open。
//   - Open（打开）：快速失败——Allow 直接返回最近一次错误（不实际执行
//     调用，避免每次请求都等待连接超时）；经过 cooldown 冷却期后 → HalfOpen。
//   - HalfOpen（半开）：放行一个探测请求（单飞，并发下同时只放行一个）；
//     探测成功 → Closed（自动恢复）；失败 → 回 Open（重置冷却）。
//
// 并发安全：状态与失败计数统一由互斥锁保护（状态切换与计数强相关，
// 用锁保持一致，不依赖原子操作）；HalfOpen 单飞通过锁内标记实现。
//
// 冷却计时采用惰性判断（Allow 时检查"上次失败时间 + cooldown <= now"），
// 无需定时器 goroutine，避免资源泄漏。
//
// 全部方法不带 context：本类型是纯内存状态机，无 IO、无自身阻塞等待
// （冷却为惰性判断，无可取消等待点），结构体不持 ctx 字段、无 Close。
// 传入 Execute 的 fn 若做 IO，由调用方闭包自带 ctx。
type Breaker struct {
	mu sync.Mutex

	state    State
	failures int // 连续失败计数（仅 classifier 判定计入的错误累加）

	threshold int           // 连续失败阈值（默认 3）
	cooldown  time.Duration // 冷却期（默认 1s）

	lastErr       error     // 最近一次失败错误（Open 快速失败时原样返回）
	lastFailTime  time.Time // 最近一次进入 Open 的时间（冷却计时起点）
	halfOpenTrial bool      // 半开探测标记：true 表示已有探测请求在途（单飞）

	classifier Classifier
}

// New 创建熔断器。非法 Option 值一律忽略，保持默认
// （阈值 3、冷却 1s、所有非 nil 错误计为失败）。
func New(opts ...Option) *Breaker {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	return &Breaker{
		state:      Closed,
		threshold:  o.threshold,
		cooldown:   o.cooldown,
		classifier: o.classifier,
	}
}

// Allow 判断请求是否允许执行：
//   - Closed → 允许（nil）
//   - Open → 冷却结束则转 HalfOpen 并放行首个探测请求；否则**快速失败**
//     （原样返回 lastErr，不包装、不引入哨兵错误）
//   - HalfOpen → 单飞：已有探测在途则拒绝（快速失败），否则放行探测
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return nil

	case Open:
		if time.Since(b.lastFailTime) >= b.cooldown {
			// 冷却结束：转半开并放行首个探测请求（标记单飞）
			b.state = HalfOpen
			b.halfOpenTrial = true
			return nil
		}
		return b.lastErr // 快速失败

	case HalfOpen:
		if b.halfOpenTrial {
			return b.lastErr // 已有探测在途：拒绝（单飞）
		}
		b.halfOpenTrial = true
		return nil
	}
	return nil
}

// Success 记录成功：重置连续失败计数；半开探测成功 → Closed（自动恢复）。
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures = 0
	b.halfOpenTrial = false
	if b.state == HalfOpen {
		b.state = Closed
		b.lastErr = nil
	}
}

// Fail 记录计为熔断的失败：连续失败达阈值 → Open（记录 lastErr 并启动冷却）；
// 半开探测失败 → 回 Open（重置冷却计时）。
func (b *Breaker) Fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == HalfOpen {
		// 探测失败：回 Open，重置冷却
		b.state = Open
		b.lastFailTime = time.Now()
		b.lastErr = err
		b.halfOpenTrial = false
		return
	}

	b.failures++
	if b.failures >= b.threshold {
		b.state = Open
		b.lastFailTime = time.Now()
		b.lastErr = err
	}
}

// Report 按 Classifier 对一次调用结果三分类记录：
//   - err == nil                → Success（半开探测成功自动闭合）；
//   - classifier(err) == true   → Fail(err)（计入熔断）；
//   - 其余（非计数错误）        → 中性：不计入也不干扰——Closed 状态下
//     保留既有连续失败计数（"连续失败"只看计数错误，与原 redis 实现
//     逐分支等价）；HalfOpen 状态下服务已被证明可达 → Closed 恢复，
//     并清零计数与探测标记（语义同 gobreaker 的 IsSuccessful）。
func (b *Breaker) Report(err error) {
	if err == nil {
		b.Success()
		return
	}
	if b.classifier(err) {
		b.Fail(err)
		return
	}

	// 非计数错误：不触发熔断；半开探测时服务可达 → 闭合恢复
	b.mu.Lock()
	if b.state == HalfOpen {
		b.state = Closed
		b.lastErr = nil
		b.halfOpenTrial = false
		b.failures = 0
	}
	b.mu.Unlock()
}

// State 返回当前状态快照：只读，不触发 Open→HalfOpen 的惰性转换
// （冷却判断只发生在 Allow 内）。
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
