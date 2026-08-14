package redis

import "github.com/redis/go-redis/v9"

type Option func(*RedisOptions)

type RedisOptions struct {
	redis.UniversalOptions
	prefix    string
	separator string
}

var (
	defaultOptions = RedisOptions{
		UniversalOptions: redis.UniversalOptions{
			Addrs: []string{"127.0.0.1:6379"},
		},
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
