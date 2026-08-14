package redis

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

var (
	NotFound = redis.Nil
)

var _ Client = &redisClient{}

type Client interface {
	redis.UniversalClient
	Constraint(...Constraint) error                             // 实例约束
	MustConstraint(constraints ...Constraint)                   // 强制约束，不符合约束条件时退出应用
	LoadFunction(f string) error                                // 加载函数脚本
	Mode() Mode                                                 // 运行模式（standalone/cluster/sentinel/ring）
	Prefix() string                                             // 统一前缀
	Separator() string                                          // 分隔符
	ComposeKey(key ...string) string                            // 组合键：拼接 key 段并应用统一前缀
	AddPrefix(prefix ...string) Client                          // 添加前缀
	ServerVersion() string                                      // 服务器版本
	Capability() *Capability                                    // 能力探测（版本、模块等）
	NewBloomFilter(key string, opts ...BloomOption) BloomFilter // 创建布隆过滤器（自动选择 BF.* 或 bitmap 实现）
	// NewBloomFilterWithEstimate 按容量与误判率创建布隆过滤器，等价于
	// NewBloomFilter(key, WithCapacity(n), WithFalsePositive(p))。
	NewBloomFilterWithEstimate(key string, capacity int64, falsePositive float64) BloomFilter
	NewCuckooFilter(key string, opts ...CuckooOption) *CuckooFilter   // 创建布谷鸟过滤器（需 RedisBloom cuckoo 模块）
	NewLock(key string, opts ...LockOption) *Lock                     // 创建分布式锁
	NewDelayedQueue(key string, opts ...QueueOption) *DelayedQueue    // 创建延迟队列（ZSET 实现）
	NewRateLimiter(name string, opts ...RateLimiterOption) *RateLimiter // 创建限流器（按名称隔离限流 key 空间，空名称不隔离）
	NewLeakyBucket(name string, opts ...LeakyBucketOption) *LeakyBucket // 创建漏桶限流器（恒定输出速率、拒绝突发；name 隔离同限流器）
	// CompareAndSet 原子比较并设置：key 当前值等于 oldValue 时设置为 newValue。
	// oldValue=nil 表示"仅当 key 不存在时设置"（SETNX 语义）。
	CompareAndSet(ctx context.Context, key string, oldValue, newValue any) (bool, error)
	// CompareAndDelete 原子比较并删除：key 当前值等于 oldValue 时删除。
	// oldValue=nil 表示"key 存在即删除"。
	CompareAndDelete(ctx context.Context, key string, oldValue any) (bool, error)
	// GracefulClose 优雅关闭连接池：幂等，级联关闭 AddPrefix 派生的子连接池，
	// 受 ctx 超时/取消控制。NewWithClient 包装的外部连接池不在此处关闭，
	// 由调用方负责在自己的生命周期内关闭。
	GracefulClose(ctx context.Context) error
}

type redisClient struct {
	redis.UniversalClient
	prefix   redisPrefix
	conf     *redis.UniversalOptions
	cap      *Capability
	state    *closeState
	ownsPool bool // 是否拥有底层连接池：NewWithClient 包装外部 uc 时为 false
	breaker  *CircuitBreaker // 熔断器（默认启用；nil 表示禁用）
}

// closeState 保存 client 连接池的生命周期状态，通过指针共享：
// redisClient 的方法均为值接收者（会复制结构体），但所有副本共享同一份
// closeState，保证关闭标记与子连接池注册表一致。
type closeState struct {
	mu       sync.Mutex
	closed   bool                      // 是否已关闭（幂等关闭标记）
	children map[*redisClient]struct{} // AddPrefix 派生的子连接池
}

func ParseURL(redisURL string, opts ...Option) (RedisOptions, error) {
	// 统一先用 net/url 解析，做两件事：
	//
	// 1. 检测 host 段是否含逗号：逗号分隔的多地址是 Redis Cluster/Sentinel
	//    的种子列表。go-redis 的 ParseClusterURL 只支持单一 host（逗号串会
	//    被当作整体地址，dial 阶段才报 lookup 失败）；用户名/密码由 url.Parse
	//    分离到 User 字段，密码中的逗号/特殊字符不会污染 host 判断。
	//
	// 2. 剥离哨兵参数 master_name：go-redis 官方无哨兵 URL 格式，其
	//    ParseClusterURL 对未知 query 参数直接报 "unexpected option"，因此
	//    必须先行剥离，解析完成后再回填 UniversalOptions.MasterName。
	//    master_name 存在即剥离（含空值，空值同样会被 ParseClusterURL 拒绝）；
	//    仅当非空时回填字段，空值按不存在处理（与空 host 列表的宽容风格一致）。
	//
	// 交互语义：
	//   - master_name 非空 → 哨兵 failover client（NewUniversalClient 按
	//     MasterName 字段自动创建）；Addrs 多地址 + master_name 是哨兵节点
	//     列表；单地址 + master_name 是单哨兵节点（failover client 同样适用）。
	//   - 多地址无 master_name → 集群 client（len(Addrs)>1 自动判断）。
	//   - 单地址无 master_name → 单机 client。
	u, err := url.Parse(redisURL)
	if err != nil {
		return RedisOptions{}, err
	}

	masterName := ""
	if q := u.Query(); q.Has("master_name") {
		masterName = q.Get("master_name")
		q.Del("master_name")
		u.RawQuery = q.Encode()
	}

	var copt RedisOptions
	if strings.Contains(u.Host, ",") {
		copt, err = parseMultiAddrURL(u, opts...)
	} else {
		copt, err = parseSingleAddrURL(u, opts...)
	}
	if err != nil {
		return RedisOptions{}, err
	}

	if masterName != "" {
		copt.MasterName = masterName
	}
	return copt, nil
}

// parseSingleAddrURL 解析单地址 URL（已剥离 master_name）。
// ParseClusterURL 返回 *redis.ClusterOptions，经 universalOptionsFromCluster
// 转为 UniversalOptions，保证 MaxRedirects/ReadOnly/RouteByLatency/
// RouteRandomly/CredentialsProvider 等字段完整拷贝（见 extract_options.go）。
func parseSingleAddrURL(u *url.URL, opts ...Option) (RedisOptions, error) {
	ropt, err := redis.ParseClusterURL(u.String())
	if err != nil {
		return RedisOptions{}, err
	}

	copt := RedisOptions{UniversalOptions: *universalOptionsFromCluster(ropt)}
	for _, o := range opts {
		o(&copt)
	}

	return copt, nil
}

// parseMultiAddrURL 解析逗号分隔的多地址集群 URL。
// 以第一个地址重建 URL 交给 ParseClusterURL 提取完整连接配置（userinfo 中的
// 用户名/密码、TLS、超时等），再把 Addrs 替换为拆分后的地址列表。
// 注意：db path（如 redis://host:6379/1）在集群场景无意义（集群仅 db0），
// ParseClusterURL 不解析 path，DB 保持零值，与单地址行为一致。
func parseMultiAddrURL(u *url.URL, opts ...Option) (RedisOptions, error) {
	hosts := strings.Split(u.Host, ",")
	addrs := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if a := strings.TrimSpace(h); a != "" {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		// 错误消息只输出 host（u.String() 含 userinfo 明文密码，记录错误会泄露）
		return RedisOptions{}, fmt.Errorf("redis: empty host list in URL host %q", u.Host)
	}

	// 以第一个地址重建 URL（保留 scheme/userinfo/query），交给 ParseClusterURL 解析
	u2 := *u
	u2.Host = addrs[0]
	ropt, err := redis.ParseClusterURL(u2.String())
	if err != nil {
		return RedisOptions{}, err
	}

	// 第一个地址以 ParseClusterURL 规范化的结果为准（无端口时补默认端口 6379），
	// 其余逗号拆分的地址保持原样，并保留 addr query 参数追加的额外地址
	// （ParseClusterURL 的 addr 参数可多次指定并追加到 Addrs）
	addrs = append([]string{ropt.Addrs[0]}, addrs[1:]...)
	if len(ropt.Addrs) > 1 {
		addrs = append(addrs, ropt.Addrs[1:]...)
	}
	ropt.Addrs = addrs

	copt := RedisOptions{UniversalOptions: *universalOptionsFromCluster(ropt)}
	for _, o := range opts {
		o(&copt)
	}

	return copt, nil
}

func NewWithUrl(url string, opts ...Option) (Client, error) {
	opt, err := ParseURL(url, opts...)
	if err != nil {
		return nil, err
	}

	return newWithOpts(&opt, newPrefix(opt.separator, opt.prefix)), nil
}

func New(opts ...Option) Client {
	opt := defaultOptions
	for _, o := range opts {
		o(&opt)
	}
	return newWithOpts(&opt, newPrefix(opt.separator, opt.prefix))
}

// NewWithClient 包装一个外部已有的 go-redis UniversalClient，返回本库的 Client。
// 会在传入的 uc 上注册前缀改写 hook（PrefixHook），前缀取自 opts 中的 WithPrefix；
// 若 opts 未提供前缀，则返回无前缀的纯包装 client（hook 直通，不影响原 client 行为）。
//
// 注意：一个 uc 只能被 NewWithClient 包装一次（go-redis 的 hook 只能追加不能移除，
// 重复包装会导致前缀 hook 叠加）。
//
// 连接池所有权：NewWithClient 不拥有传入的 uc —— GracefulClose/Close 只级联关闭
// AddPrefix 派生的子连接池，不会关闭传入的 uc，由调用方负责在适当时机关闭自己的 uc。
//
// AddPrefix 派生的子连接池：自动继承 uc 的真实连接配置（地址、密码、DB、TLS 等，
// 支持 *redis.Client/*redis.ClusterClient/*redis.Ring），保证派生池与 uc 连到同一
// 服务器；显式传入的连接 Option（WithAddr/WithRedisOptions 等）会覆盖提取出的配置。
// 若 uc 为无法提取配置的类型，则必须显式提供连接 Option，否则返回错误。
func NewWithClient(uc redis.UniversalClient, opts ...Option) (Client, error) {
	if uc == nil {
		return nil, errors.New("redis: nil UniversalClient")
	}

	opt := defaultOptions

	// 从外部 uc 提取真实连接配置作为子连接池的基础配置，避免 AddPrefix
	// 派生的子连接池静默回落到默认的 127.0.0.1:6379 而写错服务器。
	if uo := extractUniversalOptions(uc); uo != nil {
		opt.UniversalOptions = *uo
	} else {
		// 未知的 UniversalClient 实现：无法提取配置，要求显式提供连接 Option。
		return nil, errors.New("redis: unsupported UniversalClient type, " +
			"provide connection options via WithAddr/WithRedisOptions")
	}

	for _, o := range opts {
		o(&opt)
	}
	prefix := newPrefix(opt.separator, opt.prefix)

	// 注册前缀改写 hook（空前缀时直通，不影响传入 client 的行为）
	uc.AddHook(renameHook{prefix: prefix})

	client := &redisClient{
		UniversalClient: uc,
		prefix:          prefix,
		conf:            &opt.UniversalOptions,
		ownsPool:        false, // 不拥有外部传入的连接池
		state: &closeState{
			children: make(map[*redisClient]struct{}),
		},
	}
	client.cap = newCapability(client)

	// 注册熔断 hook（renameHook 之后 → 最外层，先熔断判断再前缀改写）
	client.initBreaker(uc, &opt)

	return client, nil
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

func (rdb redisClient) Constraint(constraints ...Constraint) error {
	for _, c := range constraints {
		// 传入指针：*redisClient 实现 Client 接口
		if err := c(&rdb); err != nil {
			return err
		}
	}

	return nil
}

func (rdb redisClient) MustConstraint(constraints ...Constraint) {
	for _, c := range constraints {
		if err := c(&rdb); err != nil {
			panic(err)
		}
	}
}

func (rdb redisClient) AddPrefix(prefixes ...string) Client {
	old := rdb.prefix
	p := newPrefix(old.separator, old.rename(prefixes...))

	// AddPrefix 会创建独立的连接池，父 client 必须登记该子连接池，
	// 以便父 client 关闭（GracefulClose）时级联关闭，避免连接池泄漏。
	child := newWithOpts(&RedisOptions{UniversalOptions: *rdb.conf}, p)

	rdb.state.mu.Lock()
	if rdb.state.closed {
		// 父连接池已关闭：新建的子连接池无人管理，立即关闭避免泄漏
		rdb.state.mu.Unlock()
		_ = child.GracefulClose(context.Background())
		return child
	}

	rdb.state.children[child] = struct{}{}
	rdb.state.mu.Unlock()

	return child
}

func (rdb redisClient) Prefix() string {
	return rdb.prefix.prefix
}

func (rdb redisClient) Separator() string {
	return rdb.prefix.separator
}

func (rdb redisClient) ComposeKey(key ...string) string {
	return rdb.prefix.rename(key...)
}

// LoadFunction 加载函数脚本。
// 集群环境下只加载到当前连接节点会导致其他主节点执行函数时报错，
// 因此按底层连接类型分发：
//   - *redis.ClusterClient：并发向所有主节点加载（ForEachMaster，返回首个错误）
//   - *redis.Ring：向所有 shard 实例加载（ForEachShard，函数为实例级状态，
//     路由到未加载的 shard 会执行失败）
//   - 其余（单机/哨兵 failover）：直接加载到当前连接
//
// 注：miniredis 不支持 FUNCTION 命令，单机路径验证需真实 Redis；
// 集群/Ring 路径需真实多实例环境。
func (rdb redisClient) LoadFunction(code string) error {
	ctx := context.Background()

	switch cc := rdb.UniversalClient.(type) {
	case *redis.ClusterClient:
		return cc.ForEachMaster(ctx, func(ctx context.Context, client *redis.Client) error {
			return client.FunctionLoadReplace(ctx, code).Err()
		})
	case *redis.Ring:
		return cc.ForEachShard(ctx, func(ctx context.Context, client *redis.Client) error {
			return client.FunctionLoadReplace(ctx, code).Err()
		})
	default:
		return rdb.FunctionLoadReplace(ctx, code).Err()
	}
}

func (rdb redisClient) ServerVersion() string {
	return rdb.cap.Version()
}

func (rdb redisClient) Capability() *Capability {
	return rdb.cap
}

// Close 关闭连接池。重写嵌入的 UniversalClient.Close()：
// 统一走 GracefulClose 语义（幂等 + 级联关闭 AddPrefix 派生池）。
// 对 NewWithClient 包装的外部连接池，只关闭本库派生的子连接池，
// 不关闭外部传入的 uc（由调用方负责关闭自己的连接池）。
func (rdb redisClient) Close() error {
	return rdb.GracefulClose(context.Background())
}

func (rdb redisClient) GracefulClose(ctx context.Context) error {
	// 幂等：确保自身及所有派生连接池只被关闭一次
	rdb.state.mu.Lock()
	if rdb.state.closed {
		rdb.state.mu.Unlock()
		return nil
	}
	rdb.state.closed = true

	children := make([]*redisClient, 0, len(rdb.state.children))
	for c := range rdb.state.children {
		children = append(children, c)
	}
	rdb.state.mu.Unlock()

	// 级联关闭 AddPrefix 派生的所有子连接池（递归），
	// 单个子连接池关闭失败不中断，确保全部释放
	for _, c := range children {
		_ = c.GracefulClose(ctx)
	}

	// NewWithClient 包装的外部连接池由调用方负责关闭，此处不做处理
	if !rdb.ownsPool {
		return nil
	}

	// go-redis v9 的 UniversalClient.Close() 不接受 context，
	// 因此在 goroutine 中执行连接池关闭，并通过 select 同时监听关闭结果与 ctx 信号。
	// 若 ctx 先超时/取消，返回 ctx 错误；后台 Close() 仍会继续执行
	// （done 为缓冲 channel，无需读取也不会泄漏 goroutine）。
	// 注意：必须显式调用底层 UniversalClient.Close()（而非 rdb.Close()），
	// 否则会递归调用本类型重写的 Close() 造成死循环。
	done := make(chan error, 1)
	go func() {
		done <- rdb.UniversalClient.Close()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newWithOpts(opt *RedisOptions, prefix redisPrefix) *redisClient {
	rdb := redis.NewUniversalClient(&opt.UniversalOptions)
	rdb.AddHook(renameHook{prefix: prefix})

	client := &redisClient{
		UniversalClient: rdb,
		prefix:          prefix,
		conf:            &opt.UniversalOptions,
		ownsPool:        true, // 内部创建连接池，拥有所有权
		state: &closeState{
			children: make(map[*redisClient]struct{}),
		},
	}
	client.cap = newCapability(client)

	// 注册熔断 hook：必须在 renameHook 之后注册（go-redis hook 链后注册的
	// 最外层，先熔断判断再前缀改写）
	client.initBreaker(rdb, opt)

	return client
}

// initBreaker 按配置构造熔断器并注册熔断 hook（默认启用）。
// 熔断 hook 为最外层：Open 时快速失败（不执行命令、不实际连接）。
func (c *redisClient) initBreaker(rdb redis.UniversalClient, opt *RedisOptions) {
	if !opt.breakerEnabled {
		return
	}
	c.breaker = newCircuitBreaker(opt.breakerThreshold, opt.breakerCooldown)
	rdb.AddHook(&breakerHook{breaker: c.breaker})
}
