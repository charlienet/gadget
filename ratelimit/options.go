package ratelimit

import (
	"log/slog"
	"time"
)

// FailPolicy 定义后端不可用（错误链含 ErrBackendUnavailable）时的兜底策略。
type FailPolicy uint8

const (
	// FailOpen 失效时放行：限流是保护性能力，服务不可用时宁可多放也不
	// 阻塞业务（默认值，出处对齐 redis/ratelimit.go "限流默认 FailOpen"）。
	// 与 lock 的默认（FailClosed）相反，系语义差异：锁错放行破坏互斥，
	// 限流错放行仅放大流量，调用方按 errors.Is(err, ErrFailOpen) 可感知。
	FailOpen FailPolicy = iota
	// FailClosed 失效时拒绝（返回包装错误，放行风险归零、可用性风险自担）。
	FailClosed
)

// 默认参数（对齐设计稿 §4 Option 表）。
const (
	defaultRate          = 100              // 100 个 /
	defaultPer           = time.Second      // 1 秒
	defaultMaxWait       = 30 * time.Second // Wait 总等待上限
	defaultLeaseInterval = time.Second      // 目标批发间隔
	defaultLeaseRatio    = 0.5              // 批量调节系数
	defaultIdleRetention = 60 * time.Second // 本地账本闲置回收阈值

	// backendTimeoutCap 是 WithBackendTimeout 未显式设置时的上界：
	// 默认取 min(LeaseInterval, 5s)。
	backendTimeoutCap = 5 * time.Second

	// minWaitInterval 是 RetryAfter 为 0 时的最小等待时长，防忙循环
	// （对齐 redis/ratelimit.go minWaitInterval 先例）。
	minWaitInterval = time.Millisecond
)

// options 是 New 的内部配置，仅经 Option 修改。
type options struct {
	rate     int           // Per 窗口内的令牌数
	per      time.Duration // 速率窗口
	burst    int           // 桶容量；burstSet 为 false 时取 2×Rate
	burstSet bool

	maxWait       time.Duration // Wait 总等待上限
	localLease    bool          // true = 租约模式（默认）
	leaseInterval time.Duration // 目标批发间隔
	leaseRatio    float64       // 批量调节系数

	backendTimeout    time.Duration // 内部批发 ctx 超时
	backendTimeoutSet bool

	idleRetention time.Duration // 本地账本闲置回收阈值
	policy        FailPolicy    // 后端不可用兜底策略
	logger        *slog.Logger  // 内部事件日志
	clock         Clock         // 时间源
}

// defaultOptions 返回默认配置：100/1s、burst 随最终 Rate（2×Rate）、
// 租约模式、FailOpen、真实时钟、slog.Default。
func defaultOptions() *options {
	return &options{
		rate:          defaultRate,
		per:           defaultPer,
		maxWait:       defaultMaxWait,
		localLease:    true,
		leaseInterval: defaultLeaseInterval,
		leaseRatio:    defaultLeaseRatio,
		idleRetention: defaultIdleRetention,
		policy:        FailOpen,
		logger:        slog.Default(),
		clock:         systemClock{},
	}
}

// finalize 在全部 Option 应用后推导依赖型默认值：
//   - Burst 未显式设置 → 2×Rate（跟随最终 Rate 而非初始默认）；
//   - BackendTimeout 未显式设置 → min(LeaseInterval, 5s)。
func (o *options) finalize() {
	if !o.burstSet {
		o.burst = 2 * o.rate
	}
	if !o.backendTimeoutSet {
		t := o.leaseInterval
		if t > backendTimeoutCap {
			t = backendTimeoutCap
		}
		o.backendTimeout = t
	}
}

// Option 配置 Limiter。非法值一律防御式忽略，保持默认（对齐 breaker 先例）。
type Option func(*options)

// WithRate 设置速率：Per 窗口内放行 Rate 个令牌（默认 100/1s）。
// Rate <= 0 或 per <= 0 时忽略（保持默认）。
func WithRate(n int, per time.Duration) Option {
	return func(o *options) {
		if n <= 0 || per <= 0 {
			return
		}
		o.rate = n
		o.per = per
	}
}

// WithBurst 设置桶容量（突发容忍，默认 2×Rate）。n <= 0 时忽略。
// Allow/Wait 的 n 超过 Burst 属参数错误（ErrInvalidArgument）。
func WithBurst(n int) Option {
	return func(o *options) {
		if n <= 0 {
			return
		}
		o.burst = n
		o.burstSet = true
	}
}

// WithMaxWait 设置 Wait 的总等待上限（默认 30s），超时返回 ErrExceeded
// 语义错误。d <= 0 时忽略。
func WithMaxWait(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			return
		}
		o.maxWait = d
	}
}

// WithoutLocalLease 关闭本地租约，切换为精确模式：每次 Allow/Wait 直接
// 以 GrantAllOrNothing 调用后端，严格全局配额、无本地缓存、无闲置回收
// 后台协程。默认不调用本 Option 即租约模式。
func WithoutLocalLease() Option {
	return func(o *options) {
		o.localLease = false
	}
}

// WithLeaseInterval 设置目标批发间隔（默认 1s）：租约模式按
// want = clamp(round(Rate × d / Per × LeaseRatio), 1, Burst) 计算批量，
// 期望在 d 时长内恰好零售完一次批发的存量。d <= 0 时忽略。
func WithLeaseInterval(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			return
		}
		o.leaseInterval = d
	}
}

// WithLeaseRatio 设置批量调节系数（默认 0.5）：越小越省远端调用但突发
// 上界越松散，越大越贴近精确。r <= 0 或 r > 1 时忽略。
func WithLeaseRatio(r float64) Option {
	return func(o *options) {
		if r <= 0 || r > 1 {
			return
		}
		o.leaseRatio = r
	}
}

// WithBackendTimeout 设置 core 内部批发 ctx 的超时（默认
// min(LeaseInterval, 5s)）。归 core 而非插件：它约束的是 core 发起的
// 批发调用。d <= 0 时忽略。
func WithBackendTimeout(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			return
		}
		o.backendTimeout = d
		o.backendTimeoutSet = true
	}
}

// WithIdleRetention 设置闲置回收阈值（默认 60s）——"闲置多久后条目被回收"：
//   - 本地账本条目：超过该时长无任何访问即被 sweeper 删除（未用完的租约
//     存量一并丢弃，靠后端桶自然回补）；
//   - Memory 后端的桶条目（memoryBackend.buckets）：key 超过该时长未被
//     Wholesale 访问，即被访问路径惰性 delete 或被同一 sweeper tick 的
//     reapIdle 批量 delete，下次访问重建为满桶——保证无界 key 空间下
//     map 条目数随闲置下降，内存不无界增长。
//
// 同时随 Spec.IdleRetention 下发给需要它的后端（Memory 使用，GCRA 类
// 后端忽略）。d <= 0 时忽略。
func WithIdleRetention(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			return
		}
		o.idleRetention = d
	}
}

// WithFailPolicy 设置后端不可用时的兜底策略（默认 FailOpen）。
func WithFailPolicy(p FailPolicy) Option {
	return func(o *options) {
		o.policy = p
	}
}

// WithLogger 注入内部事件日志（FailOpen 兜底等，默认 slog.Default）。
// l 为 nil 时忽略（对齐 cache WithLogger 先例；slog 属标准库，不破零依赖）。
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l == nil {
			return
		}
		o.logger = l
	}
}

// WithClock 注入时间源（默认系统时钟），测试可注入可控时钟免 sleep。
// c 为 nil 时忽略。
func WithClock(c Clock) Option {
	return func(o *options) {
		if c == nil {
			return
		}
		o.clock = c
	}
}
