package lock

import "time"

// Option 配置 Lock。
type Option func(*options)

type options struct {
	backend       Backend
	ttl           time.Duration
	retryInterval time.Duration
	timeout       time.Duration
	token         string
	policy        FailPolicy
}

const (
	defaultTTL           = 30 * time.Second
	defaultRetryInterval = 100 * time.Millisecond
)

func defaultOptions() *options {
	return &options{
		ttl:           defaultTTL,
		retryInterval: defaultRetryInterval,
		policy:        FailClosed,
	}
}

// WithBackend 注入锁后端（必选，缺失时 New panic）。nil 参数被忽略。
func WithBackend(b Backend) Option {
	return func(o *options) {
		if b != nil {
			o.backend = b
		}
	}
}

// WithTTL 设置锁的过期时间（默认 30 秒）。d <= 0 被忽略。
func WithTTL(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.ttl = d
		}
	}
}

// WithRetryInterval 设置 Lock 阻塞获取失败后的重试间隔（默认 100ms）。
func WithRetryInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.retryInterval = d
		}
	}
}

// WithTimeout 设置 Lock 阻塞获取的整体超时（0 = 无限等待，默认 0）。
func WithTimeout(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.timeout = d
		}
	}
}

// WithToken 指定锁的 token（默认随机生成）。
func WithToken(token string) Option {
	return func(o *options) {
		if token != "" {
			o.token = token
		}
	}
}

// WithFailPolicy 设置后端不可用时的兜底策略（默认 FailClosed）。
func WithFailPolicy(p FailPolicy) Option {
	return func(o *options) {
		o.policy = p
	}
}
