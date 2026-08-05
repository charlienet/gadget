package redis

import (
	"context"

	"github.com/redis/go-redis/v9"
)

const (
	defaultSlowThreshold = "5000" // 慢查询(单位微秒)
)

var (
	NotFound = redis.Nil
)

var _ Client = &redisClient{}

type Client interface {
	redis.UniversalClient
	Constraint(...constraintFunc) error           // 实例约束
	MustConstraint(constraints ...constraintFunc) // 强制约束，不符合约束条件时退出应用
	LoadFunction(f string) error                  // 加载函数脚本
	Prefix() string                               // 统一前缀
	Separator() string                            // 分隔符
	JoinKeys(key ...string) string                // 连接键
	AddPrefix(prefix ...string) *redisClient      // 添加前缀
	ServerVersion() string                        // 服务器版本
	IsStack() bool                                // 服务器环境是否为Redis stack
	Capability() *Capability                      // 能力探测（版本、模块等）
	AutoPipeline() (*redis.AutoPipeliner, error)          // 获取阻塞模式自动管道
	AsyncAutoPipeline() (*redis.AutoPipeliner, error)      // 获取异步模式自动管道
	PoolStats() *redis.PoolStats                           // 连接池统计
	GracefulClose() error                                  // 优雅关闭
}

type redisClient struct {
	redis.UniversalClient
	prefix redisPrefix
	conf   *redis.UniversalOptions
	cap    *Capability
}

func ParseURL(redisURL string, opts ...Option) (RedisOptions, error) {
	ropt, err := redis.ParseClusterURL(redisURL)
	if err != nil {
		return RedisOptions{}, err
	}

	copt := RedisOptions{UniversalOptions: redis.UniversalOptions{
		Addrs:      ropt.Addrs,
		ClientName: ropt.ClientName,
		Dialer:     ropt.Dialer,
		OnConnect:  ropt.OnConnect,

		Protocol: ropt.Protocol,
		Username: ropt.Username,
		Password: ropt.Password,

		MaxRetries:      ropt.MaxRetries,
		MinRetryBackoff: ropt.MinRetryBackoff,
		MaxRetryBackoff: ropt.MaxRetryBackoff,

		DialTimeout:           ropt.DialTimeout,
		ReadTimeout:           ropt.ReadTimeout,
		WriteTimeout:          ropt.WriteTimeout,
		ContextTimeoutEnabled: ropt.ContextTimeoutEnabled,

		PoolFIFO:         ropt.PoolFIFO,
		PoolSize:         ropt.PoolSize,
		PoolTimeout:      ropt.PoolTimeout,
		MinIdleConns:     ropt.MinIdleConns,
		MaxIdleConns:     ropt.MaxIdleConns,
		MaxActiveConns:   ropt.MaxActiveConns,
		ConnMaxIdleTime:  ropt.ConnMaxIdleTime,
		ConnMaxLifetime:  ropt.ConnMaxLifetime,
		DisableIndentity: ropt.DisableIndentity,
		IdentitySuffix:   ropt.IdentitySuffix,
		TLSConfig:        ropt.TLSConfig,
	}}
	for _, o := range opts {
		o(&copt)
	}

	return copt, nil
}

func NewWithUrl(url string, opts ...Option) (*redisClient, error) {
	opt, err := ParseURL(url, opts...)
	if err != nil {
		return nil, err
	}

	return newWithOpts(&opt, newPrefix(opt.separator, opt.prefix)), nil
}

func New(opts ...Option) *redisClient {
	opt := defaultOptions
	for _, o := range opts {
		o(&opt)
	}
	return newWithOpts(&opt, newPrefix(opt.separator, opt.prefix))
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

func (rdb redisClient) AddPrefix(prefixes ...string) *redisClient {
	old := rdb.prefix
	p := newPrefix(old.separator, old.rename(prefixes...))

	return newWithOpts(&RedisOptions{UniversalOptions: *rdb.conf}, p)
}

func (rdb redisClient) Prefix() string {
	return rdb.prefix.prefix
}

func (rdb redisClient) Separator() string {
	return rdb.prefix.separator
}

func (rdb redisClient) JoinKeys(key ...string) string {
	return rdb.prefix.rename(key...)
}

func (rdb redisClient) LoadFunction(code string) error {
	return rdb.FunctionLoadReplace(context.Background(), code).Err()
}

func (rdb redisClient) ServerVersion() string {
	return rdb.cap.Version()
}

func (rdb redisClient) IsStack() bool {
	return rdb.cap.IsStack()
}

func (rdb redisClient) Capability() *Capability {
	return rdb.cap
}

func (rdb redisClient) AutoPipeline() (*redis.AutoPipeliner, error) {
	return rdb.UniversalClient.AutoPipeline()
}

func (rdb redisClient) AsyncAutoPipeline() (*redis.AutoPipeliner, error) {
	return rdb.UniversalClient.AsyncAutoPipeline()
}

func (rdb redisClient) PoolStats() *redis.PoolStats {
	return rdb.UniversalClient.PoolStats()
}

func (rdb redisClient) GracefulClose() error {
	// 等待连接池中的请求完成后再关闭
	return rdb.UniversalClient.Close()
}

func newWithOpts(opt *RedisOptions, prefix redisPrefix) *redisClient {
	rdb := redis.NewUniversalClient(&opt.UniversalOptions)
	rdb.ConfigSet(context.Background(), "slowlog-log-slower-than", defaultSlowThreshold)
	rdb.AddHook(renameHook{prefix: prefix})

	client := &redisClient{
		UniversalClient: rdb,
		prefix:          prefix,
		conf:            &opt.UniversalOptions,
	}
	client.cap = newCapability(client)

	return client
}
