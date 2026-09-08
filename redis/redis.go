package redis

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/redis/go-redis/v9"
)

var (
	NotFound = redis.Nil
)

// IsNotFound 判断 err 是否为「键不存在」错误（即 go-redis 的 redis.Nil，含被包装后的错误）。
// 应用层用 sredis.IsNotFound(err) 判定 cache/查询 miss，无需再引入 github.com/redis/go-redis/v9。
func IsNotFound(err error) bool {
	return errors.Is(err, NotFound)
}

// IsNoGroup 判断 err 是否为 Stream 消费者组不存在错误（NOGROUP：group 或 stream 不存在），
// 命中时应（重新）创建消费组后继续消费。识别 go-redis 类型化错误，含 KVRocks 的
// "ERR " 前缀形态。应用层无需引入 github.com/redis/go-redis/v9。
func IsNoGroup(err error) bool {
	return redis.HasErrorPrefix(err, "NOGROUP")
}

// IsTxFailed 判断 err 是否为 Redis 事务（TxPipeline）WATCH 乐观锁冲突导致的失败
// （EXEC 时键被并发修改，go-redis TxFailedErr）。命中时应重放整个事务逻辑重试。
// 支持 %w 包装链。应用层无需引入 github.com/redis/go-redis/v9。
func IsTxFailed(err error) bool {
	return errors.Is(err, redis.TxFailedErr)
}

var _ Client = &redisClient{}

type Client interface {
	redis.UniversalClient
	Constraint(...Constraint) error                                                           // 实例约束
	MustConstraint(constraints ...Constraint)                                                 // 强制约束，不符合约束条件时退出应用
	LoadFunction(f string) error                                                              // 加载函数脚本
	Mode() Mode                                                                               // 运行模式（standalone/cluster/sentinel/ring）
	Prefix() string                                                                           // 统一前缀
	Separator() string                                                                        // 分隔符
	ComposeKey(key ...string) string                                                          // 组合键：拼接 key 段并应用统一前缀
	AddPrefix(prefix ...string) Client                                                        // 添加前缀
	ServerVersion() string                                                                    // 服务器版本
	Capability() *Capability                                                                  // 能力探测（版本、模块等）
	NewBloomFilter(key string, opts ...BloomOption) BloomFilter                               // 创建布隆过滤器（自动选择 BF.* 或 bitmap 实现）
	NewBloomFilterWithEstimate(key string, capacity int64, falsePositive float64) BloomFilter // 等价于 NewBloomFilter(key, WithCapacity(n), WithFalsePositive(p))。
	NewCuckooFilter(key string, opts ...CuckooOption) *CuckooFilter                           // 创建布谷鸟过滤器（需 RedisBloom cuckoo 模块）
	NewDelayedQueue(key string, opts ...QueueOption) *DelayedQueue                            // 创建延迟队列（ZSET 实现）
	NewRateLimiter(name string, opts ...RateLimiterOption) *RateLimiter                       // 创建限流器（按名称隔离限流 key 空间，空名称不隔离）；已弃用，见 ratelimit 模块，为兼容保留于接口
	NewLeakyBucket(name string, opts ...LeakyBucketOption) *LeakyBucket                       // 创建漏桶限流器（恒定输出速率、拒绝突发；name 隔离同限流器）；已弃用，见 ratelimit 模块，为兼容保留于接口
	CompareAndSet(ctx context.Context, key string, oldValue, newValue any) (bool, error)      // CompareAndSet 原子比较并设置：key 当前值等于 oldValue 时设置为 newValue。
	CompareAndDelete(ctx context.Context, key string, oldValue any) (bool, error)             // CompareAndDelete 原子比较并删除：key 当前值等于 oldValue 时删除。
	GracefulClose(ctx context.Context) error                                                  // GracefulClose 优雅关闭连接池：幂等，级联关闭 AddPrefix 派生的子连接池，
}

type redisClient struct {
	redis.UniversalClient
	prefix   redisPrefix
	conf     *redis.UniversalOptions
	cap      *Capability
	state    *closeState
	ownsPool bool            // 是否拥有底层连接池：NewWithClient 包装外部 uc 时为 false
	breaker  *CircuitBreaker // 熔断器（默认启用；nil 表示禁用）
	// luaSupport 记录服务器对 EVAL/Lua 脚本的支持情况（服务器级共享，
	// 所有扩展实例共用一份记忆）：0=未知 1=支持 -1=不支持。用指针保证
	// 值接收者的所有副本共享同一份状态（与 closeState 同理）。
	luaSupport *atomic.Int32
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
	// 逗号分隔的多地址按 cluster/sentinel 种子列表处理；master_name 是本库
	// 扩展的 query 参数（go-redis 不识别未知参数，需先行剥离再回填）。
	//
	// 交互语义：
	//   - master_name 非空 → 哨兵 failover client（多地址 = 哨兵节点列表，
	//     单地址 = 单哨兵节点）
	//   - 多地址无 master_name → 集群 client
	//   - 单地址无 master_name → 单机 client
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

// parseMultiAddrURL 解析逗号分隔的多地址集群 URL：以第一个地址重建 URL
// 提取完整连接配置，再替换为拆分后的地址列表。db path 在集群场景无意义
// （仅 db0），保持零值。
func parseMultiAddrURL(u *url.URL, opts ...Option) (RedisOptions, error) {
	hosts := strings.Split(u.Host, ",")
	addrs := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if a := strings.TrimSpace(h); a != "" {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		// 只输出 host：u.String() 含 userinfo 明文密码
		return RedisOptions{}, fmt.Errorf("redis: empty host list in URL host %q", u.Host)
	}

	// 以第一个地址重建 URL（保留 scheme/userinfo/query）解析连接配置
	u2 := *u
	u2.Host = addrs[0]
	ropt, err := redis.ParseClusterURL(u2.String())
	if err != nil {
		return RedisOptions{}, err
	}

	// 第一个地址以 ParseClusterURL 规范化结果为准（补默认端口），其余保持
	// 原样，并保留 addr query 参数追加的额外地址
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
// 若 opts 未提供前缀，则返回无前缀的纯包装 client（hook 直通）。
//
// 一个 uc 只能被 NewWithClient 包装一次（hook 只增不减，重复包装会导致
// 前缀 hook 叠加）。
//
// 连接池所有权：不拥有传入的 uc——GracefulClose/Close 只级联关闭 AddPrefix
// 派生的子连接池，不关闭 uc，由调用方负责。
//
// AddPrefix 派生的子连接池自动继承 uc 的真实连接配置（地址、密码、DB、
// TLS 等，支持 *redis.Client/*redis.ClusterClient/*redis.Ring）；显式传入的
// 连接 Option 会覆盖提取的配置。无法提取配置的类型必须显式提供连接
// Option，否则返回错误。
func NewWithClient(uc redis.UniversalClient, opts ...Option) (Client, error) {
	if uc == nil {
		return nil, errors.New("redis: nil UniversalClient")
	}

	opt := defaultOptions

	// 从外部 uc 提取真实连接配置，作为 AddPrefix 派生子连接池的基础配置
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
		luaSupport:      new(atomic.Int32),
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

	// 创建独立子连接池并登记，供父 client GracefulClose 级联关闭，
	// 避免连接池泄漏。
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

// LoadFunction 加载函数脚本。函数是实例级状态，按底层连接类型分发以保证
// 所有节点可用：
//   - *redis.ClusterClient：并发向所有主节点加载（ForEachMaster，返回首个错误）
//   - *redis.Ring：向所有 shard 实例加载（ForEachShard）
//   - 其余（单机/哨兵 failover）：直接加载到当前连接
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

	// go-redis 的 Close() 不接受 context：放 goroutine 执行，ctx 先到期则
	// 返回 ctx.Err()（后台 Close 继续完成，done 有缓冲不会泄漏）。
	// 必须调用底层 UniversalClient.Close()，直接调 rdb.Close() 会递归死循环。
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
		luaSupport:      new(atomic.Int32),
		state: &closeState{
			children: make(map[*redisClient]struct{}),
		},
	}
	client.cap = newCapability(client)

	// 熔断 hook 须在 renameHook 之后注册（后注册的最外层：先熔断判断再前缀改写）
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

// --- Lua 能力记忆 ---
//
// bitmap 类扩展优先用 Lua 脚本单往返原子完成位操作；luaSupport 记忆服务器
// 对 EVAL 的支持结果，在不支持 EVAL 的服务器（代理屏蔽、ACL 禁用等）上
// 入口直接走非原子回退路径，避免每次白付一次脚本错误往返。
// 入口分派见 bloom_bitmap.go 的 runBitmapScript。

// luaVerdict 是 EVAL 失败后的分诊结论（见 classifyLuaError）。
type luaVerdict uint8

const (
	// luaVerdictUnavailable 瞬态错误（连接/服务不可用类）：不动记忆，调用方
	// 按 FailPolicy 兜底——服务器可能只是抖动，误置 -1 会永久打入慢路径。
	luaVerdictUnavailable luaVerdict = iota
	// luaVerdictUnsupported 命令不存在/被禁类：置记忆 -1，调用方降级走
	// 非原子回退路径，此后入口跳过 EVAL。
	luaVerdictUnsupported
	// luaVerdictDataError 数据类错误（WRONGTYPE/NOPERM/位偏移非法等）：
	// 原样透传且不置记忆——与 Lua 支持与否无关。
	luaVerdictDataError
)

// classifyLuaError 对 EVAL 失败原因分诊，决定 luaSupport 的状态迁移与调用方
// 处置。判定顺序：先瞬态错误（IsUnavailable），再命令禁用类（错误文本含
// "unknown command" / "ERR unknown" / "not allowed"），其余一律视为数据类错误。
func classifyLuaError(err error) luaVerdict {
	if IsUnavailable(err) {
		return luaVerdictUnavailable
	}
	msg := err.Error()
	if strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "ERR unknown") ||
		strings.Contains(msg, "not allowed") {
		return luaVerdictUnsupported
	}
	return luaVerdictDataError
}

// luaState 返回当前 Lua 能力记忆（0=未知 1=支持 -1=不支持）；
// luaSupport 为 nil（零值构造 client）按未知处理。
func (rdb *redisClient) luaState() int32 {
	if rdb.luaSupport == nil {
		return 0
	}
	return rdb.luaSupport.Load()
}

// luaTryEval 报告入口是否应尝试 EVAL：未知/支持时尝试，不支持时跳过
// 直接走非原子回退路径。
func (rdb *redisClient) luaTryEval() bool { return rdb.luaState() != -1 }

// luaMarkSupported 记住服务器支持 Lua（仅未知态→支持一次迁移：CAS 原子
// 完成 Load-then-Store，已支持/已判不支持时免写且不覆盖 -1）。
func (rdb *redisClient) luaMarkSupported() {
	if rdb.luaSupport != nil {
		rdb.luaSupport.CompareAndSwap(0, 1)
	}
}

// luaMarkUnsupported 记住服务器不支持 Lua（此后永久走慢路径）。
func (rdb *redisClient) luaMarkUnsupported() {
	if rdb.luaSupport != nil {
		rdb.luaSupport.Store(-1)
	}
}
