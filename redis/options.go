package redis

import "github.com/redis/go-redis/v9"

type Option func(*redisOptions)

type redisOptions struct {
	redis.UniversalOptions
	perfix    string
	separator string
}

var (
	defaultOptions = redisOptions{
		UniversalOptions: redis.UniversalOptions{
			Addrs: []string{"127.0.0.1:6379"},
		},
	}
)

func WithRedisOptions(options redis.UniversalOptions) Option {
	return func(ro *redisOptions) {
		ro.UniversalOptions = options
	}
}

func WithAddr(addr string) Option {
	return func(o *redisOptions) {
		o.Addrs = []string{addr}
	}
}

func WithAddrs(addrs []string) Option {
	return func(o *redisOptions) {
		o.Addrs = addrs
	}
}

func WithPassword(password string) Option {
	return func(ro *redisOptions) {
		if len(password) > 0 {
			ro.Password = password
		}
	}
}

func WithDB(db int) Option {
	return func(ro *redisOptions) {
		ro.DB = db
	}
}

func WithPoolSize(size int) Option {
	return func(ro *redisOptions) {
		ro.PoolSize = size
	}
}

func WithPrefix(prefix string) Option {
	return func(o *redisOptions) {
		o.perfix = prefix
	}
}
