package redis

import (
	"time"

	"github.com/redis/go-redis/v9"
)

type Option func(*RedisOptions)

type RedisOptions struct {
	redis.UniversalOptions
	prefix    string
	separator string

	// 熔断器配置（默认启用：阈值 3、冷却 1s）
	breakerEnabled   bool
	breakerThreshold int
	breakerCooldown  time.Duration
}

var (
	defaultOptions = RedisOptions{
		UniversalOptions: redis.UniversalOptions{
			Addrs: []string{"127.0.0.1:6379"},
		},
		breakerEnabled: true, // 熔断器默认启用（WithBreaker(false) 显式关闭）
	}
)

func WithRedisOptions(options redis.UniversalOptions) Option {
	return func(ro *RedisOptions) {
		ro.UniversalOptions = options
	}
}

func WithAddr(addr string) Option {
	return func(o *RedisOptions) {
		o.Addrs = []string{addr}
	}
}

func WithAddrs(addrs []string) Option {
	return func(o *RedisOptions) {
		o.Addrs = addrs
	}
}

func WithPassword(password string) Option {
	return func(ro *RedisOptions) {
		if len(password) > 0 {
			ro.Password = password
		}
	}
}

func WithDB(db int) Option {
	return func(ro *RedisOptions) {
		ro.DB = db
	}
}

func WithPoolSize(size int) Option {
	return func(ro *RedisOptions) {
		ro.PoolSize = size
	}
}

func WithPrefix(prefix string) Option {
	return func(o *RedisOptions) {
		o.prefix = prefix
	}
}

// WithSeparator 设置键前缀分隔符（默认 ":"），与 WithPrefix 配套使用。
func WithSeparator(separator string) Option {
	return func(o *RedisOptions) {
		o.separator = separator
	}
}

// WithProtocol 设置 RESP 协议版本（2 或 3）
// 客户端缓存需要 Protocol: 3
func WithProtocol(protocol int) Option {
	return func(ro *RedisOptions) {
		ro.Protocol = protocol
	}
}

// WithClientSideCache 启用客户端缓存（实验性功能）
// 要求：Protocol 必须为 3，仅支持独立客户端，仅支持 DB 0
func WithClientSideCache(config *redis.ClientSideCacheConfig) Option {
	return func(ro *RedisOptions) {
		ro.ClientSideCacheConfig = config
		if ro.Protocol == 0 {
			ro.Protocol = 3
		}
	}
}

// WithAutoPipeline 设置自动管道的默认配置（实验性功能）
// 设置后，通过 AutoPipeline() 或 AsyncAutoPipeline() 获取管道实例
func WithAutoPipeline(opts *redis.AutoPipelineOptions) Option {
	return func(ro *RedisOptions) {
		ro.AutoPipelineOptions = opts
	}
}

// WithBreaker 启用/关闭熔断器（默认启用）。启用后 Redis 服务失效达到
// 连续失败阈值即进入 Open 状态快速失败，冷却后半开放行探测，成功自动恢复。
func WithBreaker(enable bool) Option {
	return func(ro *RedisOptions) {
		ro.breakerEnabled = enable
	}
}

// WithBreakerThreshold 设置熔断的连续失败阈值（默认 3；n<=0 时用默认值）。
// 仅连接类错误（IsUnavailable 判定）计数，命令级错误不计入。
func WithBreakerThreshold(n int) Option {
	return func(ro *RedisOptions) {
		ro.breakerThreshold = n
	}
}

// WithBreakerCooldown 设置熔断冷却期（默认 1s；d<=0 时用默认值）。
// 冷却期短，保证 Open 后能尽快进入半开并放行探测请求（快速重连语义）；
// 探测成功即自动闭合恢复。
func WithBreakerCooldown(d time.Duration) Option {
	return func(ro *RedisOptions) {
		ro.breakerCooldown = d
	}
}
