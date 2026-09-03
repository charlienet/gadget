package retry

import (
	"time"
)

// retryAll 是默认的可重试判定：全部错误可重试。
func retryAll(error) bool { return true }

// options 是 Do 的内部配置，仅经 Option 修改。
type options struct {
	backoff     func() Backoff   // 工厂函数：每次 Do 调用内部创建独立实例
	maxAttempts int              // fn 总执行次数上限（含首次）
	maxElapsed  time.Duration    // 软时限；<=0 表示禁用
	retryable   func(error) bool // 错误可重试判定
}

// defaultOptions 返回默认配置：指数退避工厂（100ms×2 封顶 30s）、
// 5 次尝试、无时限、全部错误可重试。
//
// 注意：backoff 以工厂函数形式存放，禁止在构造期创建共享 Backoff 实例
// ——共享实例在并发 Do 下会产生数据竞争。
func defaultOptions() *options {
	return &options{
		backoff: func() Backoff {
			return Exponential(100*time.Millisecond, 2, 30*time.Second)
		},
		maxAttempts: 5,
		maxElapsed:  0,
		retryable:   retryAll,
	}
}

// Option 配置 Do 的行为。
type Option func(*options)

// WithBackoff 指定退避策略。b 为 nil 时忽略（保持默认指数退避）。
//
// 传入的实例仅在单次 Do 内使用（Do 入口会调用其 Reset）；
// 多个 goroutine 并发 Do 时请各自构造，不要共享同一实例。
func WithBackoff(b Backoff) Option {
	return func(o *options) {
		if b == nil {
			return
		}
		o.backoff = func() Backoff { return b }
	}
}

// WithMaxAttempts 设置 fn 的总执行次数上限（含首次执行）。
//
// 例如 n=3 表示 fn 最多被调用 3 次（首次 + 至多 2 次重试）。
// n <= 0 时忽略（保持默认 5）。
func WithMaxAttempts(n int) Option {
	return func(o *options) {
		if n <= 0 {
			return
		}
		o.maxAttempts = n
	}
}

// WithMaxElapsed 设置重试总耗时的软上限。
//
// 检查发生在每轮尝试之前：已超过上限则不再重试。睡眠不被截断，
// 因此实际耗时可能超出 MaxElapsed 至多一个退避间隔。
// d <= 0 时忽略（视为禁用，与默认一致）。
// 与 MaxAttempts 无优先关系：每轮都检查，先到先终止。
func WithMaxElapsed(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			return
		}
		o.maxElapsed = d
	}
}

// WithRetryable 指定错误可重试判定：返回 false 的错误立即终止并原样
// 返回该错误。fn 为 nil 时忽略（保持默认：全部错误可重试）。
//
// 与 gadget/redis 对接示例：
//
//	retry.WithRetryable(func(err error) bool {
//		return redis.IsUnavailable(err) // 仅连接/服务类故障重试
//	})
func WithRetryable(fn func(error) bool) Option {
	return func(o *options) {
		if fn == nil {
			return
		}
		o.retryable = fn
	}
}
