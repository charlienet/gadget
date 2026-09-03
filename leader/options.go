package leader

import (
	"time"
)

// options 是 Elector 的内部配置，仅经 Option 修改，由 New 校验后定型。
type options struct {
	locker        Locker
	identity      string
	leaseDuration time.Duration
	renewDeadline time.Duration
	retryPeriod   time.Duration
	callbacks     Callbacks
}

// defaultOptions 返回 client-go 同款默认参数与 "hostname-pid" 身份。
func defaultOptions() *options {
	return &options{
		identity:      defaultIdentity(),
		leaseDuration: defaultLeaseDuration,
		renewDeadline: defaultRenewDeadline,
		retryPeriod:   defaultRetryPeriod,
	}
}

// Option 配置 Elector。非法值静默忽略（保持默认），与 retry 包 Option
// 惯例一致；必选项缺失或参数矛盾由 New 在构造期 panic 兜底。
type Option func(*options)

// WithLocker 注入锁能力（必选，缺失时 New panic）。nil 忽略。
// 生产装配即 lock.New(key, lock.WithBackend(...), lock.WithTTL(d))，
// 其中 WithTTL 应设置为与 WithLeaseDuration 相同的值（获锁瞬间的租约
// 长度由 lock 实例决定，续约长度由 LeaseDuration 决定，两者一致才能
// 保证租约语义连续）。
func WithLocker(l Locker) Option {
	return func(o *options) {
		if l == nil {
			return
		}
		o.locker = l
	}
}

// WithIdentity 设置本节点竞选身份（默认 "hostname-pid"）。空串忽略。
// 身份仅用于日志/观测自描述：受 lock 抽象限制（无持有者探查能力），
// 本模块无法感知其他竞选者的身份。
func WithIdentity(id string) Option {
	return func(o *options) {
		if id == "" {
			return
		}
		o.identity = id
	}
}

// WithCallbacks 设置生命周期回调。OnStartedLeading 为 nil 时 New panic
// （构造期编程错误 fail-fast）；其余回调 nil 跳过。
func WithCallbacks(c Callbacks) Option {
	return func(o *options) { o.callbacks = c }
}

// WithLeaseDuration 设置租约时长（默认 15s）：续约时将锁租约延长至该值，
// 也是本节点失联后其他竞选者理论上等待接管的上限。d <= 0 忽略。
// 必须满足 LeaseDuration > RenewDeadline，否则 New panic。
func WithLeaseDuration(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			return
		}
		o.leaseDuration = d
	}
}

// WithRenewDeadline 设置续约预算（默认 10s）：距上一次续约成功超过该
// 时长仍未成功，则在租约到期前主动让位（宁可误让位不可双主）。
// 必须满足 LeaseDuration > RenewDeadline > RetryPeriod，否则 New panic。
// d <= 0 忽略。
func WithRenewDeadline(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			return
		}
		o.renewDeadline = d
	}
}

// WithRetryPeriod 设置竞选轮询与续约的基准间隔（默认 2s，实际间隔带
// [1.0, 1.25) 倍抖动）。d <= 0 忽略。必须满足 RenewDeadline >
// RetryPeriod，否则 New panic。
func WithRetryPeriod(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			return
		}
		o.retryPeriod = d
	}
}
