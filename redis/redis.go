package redis

import (
	"context"
	"strings"

	"github.com/redis/go-redis/v9"
)

const (
	defaultSlowThreshold = "5000" // 慢查询(单位微秒)
)

var (
	NotFound = redis.Nil
)

var _ Client = redisClient{}

type Client interface {
	redis.UniversalClient
	Constraint(...constraintFunc) error           // 实例约束
	MustConstraint(constraints ...constraintFunc) // 强制约束，不符合约束条件时退出应用
	LoadFunction(f string)                        // 加载函数脚本
	Prefix() string                               // 统一前缀
	Separator() string                            // 分隔符
	AddPrefix(prefix ...string) redisClient       // 添加前缀
	ServerVersion() string                        // 服务器版本
	IsStack() bool                                // 服务器环境是否为Redis stack
}

type redisClient struct {
	redis.UniversalClient
	prefix redisPrefix
	conf   *redis.UniversalOptions
}

func New(opts ...Option) redisClient {
	opt := defaultOptions
	for _, o := range opts {
		o(&opt)
	}

	return new(&opt.UniversalOptions, newPrefix(opt.separator, opt.perfix))
}

func (rdb redisClient) Subscribe(ctx context.Context, channels ...string) *redis.PubSub {
	return rdb.UniversalClient.Subscribe(ctx, rdb.prefix.renames(channels...)...)
}

func (rdb redisClient) PSubscribe(ctx context.Context, channels ...string) *redis.PubSub {
	return rdb.UniversalClient.PSubscribe(ctx, rdb.prefix.renames(channels...)...)

}

func (rdb redisClient) SSubscribe(ctx context.Context, channels ...string) *redis.PubSub {
	return rdb.UniversalClient.SSubscribe(ctx, rdb.prefix.renames(channels...)...)
}

func (rdb redisClient) Constraint(constraints ...constraintFunc) error {
	for _, c := range constraints {
		if err := c(rdb); err != nil {
			return err
		}
	}

	return nil
}

func (rdb redisClient) MustConstraint(constraints ...constraintFunc) {
	for _, c := range constraints {
		if err := c(rdb); err != nil {
			panic(err)
		}
	}
}

func (rdb redisClient) AddPrefix(prefixes ...string) redisClient {
	old := rdb.prefix
	p := newPrefix(old.separator, old.rename(prefixes...))

	return new(rdb.conf, p)
}

func (rdb redisClient) Prefix() string {
	return rdb.prefix.prefix
}

func (rdb redisClient) Separator() string {
	return rdb.prefix.separator
}

func (rdb redisClient) LoadFunction(code string) {
	err := rdb.FunctionLoadReplace(context.Background(), code).Err()
	if err != nil {
		panic(err)
	}
}

func (rdb redisClient) ServerVersion() string {
	info, err := rdb.Info(context.Background(), "Server").Result()
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(info, "\r\n") {
		after, found := strings.CutPrefix(line, "redis_version:")
		if found {
			return after
		}
	}

	return ""
}

func (rdb redisClient) IsStack() bool {

	info, err := rdb.Info(context.Background(), "Modules").Result()
	if err != nil {
		return false
	}

	return len(info) > 20
}

func new(conf *redis.UniversalOptions, prefix redisPrefix) redisClient {
	rdb := redis.NewUniversalClient(conf)
	rdb.ConfigSet(context.Background(), "slowlog-log-slower-than", defaultSlowThreshold)
	rdb.AddHook(renameHook{prefix: prefix})

	return redisClient{
		UniversalClient: rdb,
		prefix:          prefix,
		conf:            conf,
	}
}
