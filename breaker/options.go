package breaker

import (
	"time"
)

// 熔断器默认参数。
const (
	defaultThreshold = 3
	defaultCooldown  = time.Second
)

// classifyAll 是默认 Classifier：所有非 nil 错误计为熔断失败。
func classifyAll(err error) bool { return err != nil }

// options 是 New 的内部配置，仅经 Option 修改。
type options struct {
	threshold  int           // 连续失败阈值
	cooldown   time.Duration // Open 后的冷却期
	classifier Classifier    // 失败判定（true = 计入熔断）
}

// defaultOptions 返回默认配置：阈值 3、冷却 1s、
// 所有非 nil 错误计为失败。
func defaultOptions() *options {
	return &options{
		threshold:  defaultThreshold,
		cooldown:   defaultCooldown,
		classifier: classifyAll,
	}
}

// Option 配置 Breaker 的行为。
type Option func(*options)

// WithThreshold 设置连续失败阈值：计数达到 n 时 Closed → Open。
// n <= 0 时忽略（保持默认 3）。
func WithThreshold(n int) Option {
	return func(o *options) {
		if n <= 0 {
			return
		}
		o.threshold = n
	}
}

// WithCooldown 设置 Open 后的冷却期：期满时 Allow 转 HalfOpen
// 放行探测请求。d <= 0 时忽略（保持默认 1s）。
// 冷却为惰性判断（无定时器 goroutine 主动唤醒），由 Allow 调用触发。
func WithCooldown(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			return
		}
		o.cooldown = d
	}
}

// WithClassifier 指定熔断失败判定：c 返回 true 的错误计入连续失败；
// 未计入的错误不触发熔断，且半开探测期间视为服务可达（Report 会闭合恢复）。
// c 为 nil 时忽略（保持默认：所有非 nil 错误计为失败）。
//
// 与 gadget/redis 对接示例（仅连接/服务类故障计入熔断）：
//
//	breaker.New(breaker.WithClassifier(redis.IsUnavailable))
func WithClassifier(c Classifier) Option {
	return func(o *options) {
		if c == nil {
			return
		}
		o.classifier = c
	}
}
