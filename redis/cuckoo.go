package redis

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeebo/xxh3"
)

// CuckooInfo 包含布谷鸟过滤器的元数据。模块版对应 CF.INFO 输出；回退版
// 仅 Size/NumBuckets/NumItems/BucketSize 有效，其余字段
// （NumFilters/NumDeletes/Expansion/MaxIterations）恒 0。
type CuckooInfo struct {
	Size          int64 // 过滤器大小（字节；回退版为估算：占用桶 × 桶字节数）
	NumBuckets    int64 // 桶数量（回退版为占用桶数）
	NumFilters    int64 // 过滤器数量（仅模块版；回退版恒 0）
	NumItems      int64 // 已插入元素数
	NumDeletes    int64 // 已删除元素数（仅模块版；回退版恒 0）
	Expansion     int64 // 扩容因子（仅模块版；回退版恒 0）
	BucketSize    int64 // 桶大小
	MaxIterations int64 // 最大踢出迭代次数（仅模块版；回退版恒 0）
}

// CuckooOption 配置布谷鸟过滤器。
type CuckooOption func(*cuckooConfig)

type cuckooConfig struct {
	failPolicyConfig
	capacity      int64 // 预估容量（模块版 >0 时 CF.RESERVE 预分配；回退版决定桶数量）
	maxIterations int64 // 最大踢出迭代次数
	bucketSize    int64 // 桶大小
	expansion     int64 // 扩容因子（仅模块版 CF.RESERVE 使用）
}

// CuckooConfig 是 CuckooFilter 的配置类型别名，供 WithFailPolicy 泛型参数使用。
type CuckooConfig = cuckooConfig

func defaultCuckooConfig() cuckooConfig { return cuckooConfig{} }

// WithCuckooCapacity 设置预估容量（命名避免与 BloomFilter 的 WithCapacity
// 冲突——两者是不同类型 Option，Go 同包不允许同名重载）。
// 模块版：指定后 CF.RESERVE 预分配；未指定（0）时 CF.ADD 惰性创建。
// 回退版：容量决定桶数量（numBuckets = capacity / bucketSize）。
func WithCuckooCapacity(n int64) CuckooOption {
	return func(c *cuckooConfig) {
		if n > 0 {
			c.capacity = n
		}
	}
}

// WithMaxIterations 设置最大踢出迭代次数（驱逐置换的上限，仅与
// WithCuckooCapacity 配合生效；模块版对应 CF.RESERVE MAXITERATIONS）。
func WithMaxIterations(n int64) CuckooOption {
	return func(c *cuckooConfig) {
		if n > 0 {
			c.maxIterations = n
		}
	}
}

// WithBucketSize 设置桶大小（仅与 WithCuckooCapacity 配合生效）。
func WithBucketSize(n int64) CuckooOption {
	return func(c *cuckooConfig) {
		if n > 0 {
			c.bucketSize = n
		}
	}
}

// WithExpansion 设置扩容因子（仅模块版 CF.RESERVE 使用；回退版无扩容概念，忽略）。
func WithExpansion(n int64) CuckooOption {
	return func(c *cuckooConfig) {
		if n > 0 {
			c.expansion = n
		}
	}
}

// cuckooFilterImpl 是布谷鸟过滤器的实现接口：模块版（cfCmdImpl）与
// 无模块回退版（hashImpl）各自实现，由 NewCuckooFilter 按能力分派。
// item 为 any：cfCmdImpl 把原始 item 透传给 CF.* 模块命令（go-redis
// writer 序列化）；hashImpl 先经 marshalItem 编码为规范字节再算指纹与
// 候选桶（格式对齐 go-redis writer 并冻结，见 marshal.go）。
type cuckooFilterImpl interface {
	Add(ctx context.Context, item any) (bool, error)
	Exists(ctx context.Context, item any) (bool, error)
	ExistsMulti(ctx context.Context, items ...any) ([]bool, error)
	// AddMulti 批量添加多个条目，结果与入参顺序一一对应（对齐 CF.INSERT 语义）。
	AddMulti(ctx context.Context, items ...any) ([]bool, error)
	Count(ctx context.Context, item any) (int64, error)
	AddNX(ctx context.Context, item any) (bool, error)
	Del(ctx context.Context, item any) (bool, error)
	Info(ctx context.Context) (*CuckooInfo, error)
	// Reset 清空整个过滤器（DEL 物理键）。模块版需同步复位 CF.RESERVE 闸门。
	Reset(ctx context.Context) error
}

// CuckooFilter 是布谷鸟过滤器的统一门面，按服务器能力自动分派实现：
//   - 服务器加载了 RedisBloom 的 cuckoo 模块 → cfCmdImpl（原生 CF.* 命令）
//   - 未加载 → hashImpl（Hash + Lua 回退，普通 Redis 即可运行，无模块依赖）
//
// 与 BloomFilter 不同，布谷鸟过滤器支持删除（Del），且误判率更低。
//
// 本库不封装 CF.INSERT/CF.INSERTNX：预分配一律经 WithCuckooCapacity
// 惰性 CF.RESERVE；回退路径首写天然 autocreate。
//
// item 序列化承诺：item 为 any。回退版（hashImpl）的指纹/桶索引由
// marshalItem(item) 的规范字节经 xxh3-64 导出；模块版（cfCmdImpl）由
// go-redis writer 序列化原始 item。marshalItem 与 writer 逐类型对齐并
// **冻结**（见 marshal.go），两路径同一 item 的字节口径一致；存量过滤器
// 数据的有效性依赖该格式永久不变，格式变更属 breaking change。不支持的
// 类型（含指针变体）返回数据类错误（不 panic、不触发 FailPolicy 兜底）。
//
// 使用示例：
//
//	cf := rdb.NewCuckooFilter("cf:1", redis.WithCuckooCapacity(1000000))
//	if ok, err := cf.Add(ctx, "item1"); err != nil {
//		return err
//	}
//	exists, err := cf.Exists(ctx, "item1")
//	if err := cf.Del(ctx, "item1"); err != nil {
//		return err
//	}
type CuckooFilter struct {
	impl   cuckooFilterImpl
	policy FailPolicy // 失效兜底策略（默认 FailOpen）
}

// NewCuckooFilter 创建布谷鸟过滤器（挂 *redisClient）。
// 分派逻辑：每次创建时按能力探测结果选择实现（Capability().HasCuckoo()，
// 探测结果有缓存，与 bloom.go 的 NewBloomFilter 分派方式一致）。
// 失效兜底策略默认 FailOpen（过滤器是保护性能力：服务不可用时放行业务）；
// 可用 WithFailPolicy 显式改为 FailClosed。
//
// ⚠️ v0.7.0 行为变更（BREAKING）：HasCuckoo 自本版起经
// `COMMAND INFO CF.ADD` 真实确认命令族可用性；此前误把命令前缀 "cf" 当
// 模块名查 INFO MODULES，**恒 false**，导致有 CF.* 的服务器静默回退到
// Lua 回退实现。修复后这类服务器会自动改分派 CF.* 原生路径——两实现的
// 键结构完全不同（CF.* 的模块内部编码 vs 回退版的单个 Hash key），
// **数据不互通**：升级前用回退版写入的过滤器，升级后在 CF.* 路径下读不到，
// 需按新 key 重建过滤器（或显式接受一次冷启动）。
func (rdb *redisClient) NewCuckooFilter(key string, opts ...CuckooOption) *CuckooFilter {
	cfg := defaultCuckooConfig()
	cfg.policy = FailOpen // 过滤器默认 FailOpen：宁可放行不阻塞业务
	for _, o := range opts {
		o(&cfg)
	}

	if rdb.cap.HasCuckoo() {
		impl := &cfCmdImpl{client: rdb, key: key, cfg: cfg}
		// 必须显式 Store：atomic.Pointer 零值 Load() 返回 nil，漏 Store 会在
		// ensureReserve 处 panic。
		impl.once.Store(new(sync.Once))
		return &CuckooFilter{impl: impl, policy: cfg.policy}
	}
	return &CuckooFilter{impl: newHashImpl(rdb, key, cfg), policy: cfg.policy}
}

// fallbackBool 按策略返回布谷鸟过滤器兜底值 + 哨兵错误：FailOpen → true
// （视为成功）；FailClosed → false。错误为 ErrRedisUnavailable 包装。
func (cf *CuckooFilter) fallbackBool(err error) (bool, error) {
	if cf.policy == FailOpen {
		return true, fallbackErr(err)
	}
	return false, fallbackErr(err)
}

// fallbackBools 返回 ExistsMulti 的兜底切片：FailOpen → 全 true；
// FailClosed → 全 false。禁止真实/兜底混合结果（对齐 bloom_bf.go 的
// fallbackBools 语义）。n 为入参 item 数。
func (cf *CuckooFilter) fallbackBools(n int, err error) ([]bool, error) {
	res := make([]bool, n)
	for i := range res {
		res[i] = cf.policy == FailOpen
	}
	return res, fallbackErr(err)
}

// Add 向过滤器添加一个元素，返回是否插入。**两路径语义不一致**——
// 模块版为多重集插入（已存在也会再插一份，false 源于桶满/驱逐超限）；
// 回退版为去重式（false 即已存在）。跨路径勿依赖重复 Add 增值；需要
// 唯一保证用 AddNX，观察次数用 Count，正规批量用 AddMulti。
// IsUnavailable 时按 FailPolicy 兜底且返回 ErrRedisUnavailable 哨兵
// （errors.Is 可感知）；数据类错误恒原样返回。
func (cf *CuckooFilter) Add(ctx context.Context, item any) (bool, error) {
	added, err := cf.impl.Add(ctx, item)
	if err != nil && IsUnavailable(err) {
		return cf.fallbackBool(err)
	}
	return added, err
}

// Exists 检查元素是否可能存在于过滤器（布谷鸟过滤器无假阴性，可能有假阳性）。
// Redis 服务失效时按兜底策略：FailOpen → (true, nil)（视为存在，防穿透失效
// 但放行业务）；FailClosed → (false, nil)。
// item 属不支持类型时返回数据类错误，不触发兜底。
func (cf *CuckooFilter) Exists(ctx context.Context, item any) (bool, error) {
	exists, err := cf.impl.Exists(ctx, item)
	if err != nil && IsUnavailable(err) {
		return cf.fallbackBool(err)
	}
	return exists, err
}

// ExistsMulti 批量检查多个元素是否存在，结果与入参顺序一一对应；
// 存在性语义同 Exists（无假阴性，可能假阳性）。单命令/单脚本往返。
// items 为空时返回 (nil, nil)。
// IsUnavailable 时整体按 FailPolicy 兜底（FailOpen 全 true / FailClosed
// 全 false）且返回 ErrRedisUnavailable 哨兵（errors.Is 可感知），禁止
// 部分真实部分兜底的混合结果；数据类错误恒原样返回。
func (cf *CuckooFilter) ExistsMulti(ctx context.Context, items ...any) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	res, err := cf.impl.ExistsMulti(ctx, items...)
	if err != nil {
		if IsUnavailable(err) {
			// 兜底长度以入参数为准（err 路径下 res 可能为 nil 或残缺）
			return cf.fallbackBools(max(len(res), len(items)), err)
		}
		return nil, err
	}
	return res, nil
}

// Count 返回元素在过滤器中的出现次数估计。0 表示确定不存在（无假阴性）；
// >0 为出现次数估计，可能因指纹碰撞高估。模块版（CF.ADD 多重集语义）
// Count 可取任意值；回退版（Add 去重语义）恒为 0/1。跨路径勿依赖精确计数。
// IsUnavailable 时返回 (0, ErrRedisUnavailable 哨兵)（errors.Is 可感知），
// 观测类不随 FailPolicy 分叉；数据类错误恒原样返回。
func (cf *CuckooFilter) Count(ctx context.Context, item any) (int64, error) {
	n, err := cf.impl.Count(ctx, item)
	if err != nil {
		if IsUnavailable(err) {
			// 观测类：零值 + 哨兵错误，不做 FailOpen "放行"语义
			return 0, fallbackErr(err)
		}
		return 0, err
	}
	return n, nil
}

// AddNX 仅在元素不存在时插入，返回是否实际插入。需要唯一性/去重保证时
// 一律用本方法（模块版 Add 是多重集插入——已存在也会再插一份；AddNX 恒
// "存在即不加"；回退版两方法等价，Add 本就是去重式）。
// false 的原因不承诺可区分：已存在、或过滤器满/驱逐超限均返回 false。
// IsUnavailable 时按 FailPolicy 兜底且返回 ErrRedisUnavailable 哨兵
// （errors.Is 可感知）；数据类错误恒原样返回。
func (cf *CuckooFilter) AddNX(ctx context.Context, item any) (bool, error) {
	added, err := cf.impl.AddNX(ctx, item)
	if err != nil && IsUnavailable(err) {
		return cf.fallbackBool(err)
	}
	return added, err
}

// AddMulti 批量添加多个条目，结果与入参顺序一一对应（对齐 CF.INSERT 语义）。
// 每元素的 bool 语义与 Add 完全相同：模块版为多重集插入达成与否（已存在
// 也再插一份，false 源于桶满/驱逐超限）；回退版为去重新增与否（false 即
// 已存在）。items 为空时返回 (nil, nil)。
//
// ⚠️ 重试警告：失败时可能部分写入；模块版重试会把已插入项再插一份
// （多重集），Count 计数翻倍、返回值非终态——重试前请确认可接受重复插入；
// 需要幂等回填的场景用 AddNX 逐条或先 ExistsMulti 去重。
//
// 成本：回退版为单条 Lua，时长 O(n×maxIterations×bucketSize)，超大批量
// 会阻塞 Redis，由调用方控批（对齐 bloom bitmapAddMulti 纪律，不分块）。
//
// IsUnavailable 时整体按 FailPolicy 兜底（FailOpen 全 true / FailClosed
// 全 false）且返回 ErrRedisUnavailable 哨兵（errors.Is 可感知），禁止混合
// 结果；数据类错误恒原样返回（但同样可能已部分写入，见上方重试警告）。
func (cf *CuckooFilter) AddMulti(ctx context.Context, items ...any) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	res, err := cf.impl.AddMulti(ctx, items...)
	if err != nil {
		if IsUnavailable(err) {
			// 兜底长度以入参数为准（err 路径下 res 可能为 nil 或残缺）
			return cf.fallbackBools(max(len(res), len(items)), err)
		}
		return nil, err
	}
	return res, nil
}

// Del 从过滤器删除一个元素。注意与 BF 不同：CF.DEL 返回是否删除成功
// （元素不存在或删除导致桶耗尽时返回 false）。
// 键整体不存在时模块版 CF.DEL 原生报 "Not found" 命令错误、回退版返回
// (false, nil)——门面将其归一为 (false, nil)：Del/Reset 组合等场景下
// 两路径语义对齐是包级契约，调用方无需按能力分派处理错误。归一仅在
// 门面层，底层 impl 保留真实语义（白盒测试可观测原生行为）。
// Del 需持有原文（item 本体）且过滤器无枚举能力，无法用于清空；
// 整键清空请用 Reset。
// Redis 服务失效时按兜底策略：FailOpen → (true, nil)（视为删除成功）；
// FailClosed → (false, nil)。
// item 属不支持类型时返回数据类错误，不触发兜底。
func (cf *CuckooFilter) Del(ctx context.Context, item any) (bool, error) {
	deleted, err := cf.impl.Del(ctx, item)
	if err != nil && strings.Contains(err.Error(), "Not found") {
		// not-found 归一化（评审定稿 M1）：字形取真机实测的模块输出
		// （CF.DEL 报 "Not found"，大写 N；勿与 bloom 路径小写 "not
		// found" 混淆）。置于 Unavailable 判定之前，仅字形匹配、其余
		// 错误恒透传。
		return false, nil
	}
	if err != nil && IsUnavailable(err) {
		return cf.fallbackBool(err)
	}
	return deleted, err
}

// Info 返回过滤器的元数据。回退版仅 Size/NumBuckets/NumItems/BucketSize
// 四字段有效（其余恒 0，见 CuckooInfo）。
// Redis 服务失效时按兜底策略：FailOpen → 空结构体 nil；FailClosed → 返回错误。
func (cf *CuckooFilter) Info(ctx context.Context) (*CuckooInfo, error) {
	info, err := cf.impl.Info(ctx)
	if err != nil && IsUnavailable(err) {
		// Info 非关键：兜底返回空结构体 + 哨兵错误（errors.Is 可感知）
		return &CuckooInfo{}, fallbackErr(err)
	}
	return info, err
}

// Reset 清空整个过滤器：模块版删除 CF.* 键，回退版删除整 Hash。单键
// 单命令，Redis 侧天然原子——不存在 bloom 分片形态的"部分清空可见"。
// 键不存在时返回 nil（幂等），可安全重复/失败重试。
//
// 与 Del 的边界：Del 删除单个已知原文的条目（需持有 item；无枚举能力，
// 无法全清）；Reset 整键销毁，语义等价于重建过滤器。
// Reset 不承诺与并发写入的相对次序（并发 Add 的元素可能落在清空前或
// 清空后的过滤器世代）。
//
// Reset ≠ CF.COMPACT：前者销毁全部数据，后者是 RedisBloom 的内部整理
// 命令、数据保留（本仓库未封装 CF.COMPACT，Reset 不隐式调用它）。
//
// 失败（含服务不可用）恒返回错误，不受 FailPolicy 兜底影响——"没清掉
// 却假装清了"会让会话隔离静默失效。
//
// 模块版注意：Reset 会复位内部惰性 CF.RESERVE 闸门，之后继续 Add 按
// WithCuckooCapacity 等配置重建过滤器；绕开本方法手工 DEL 后复用实例，
// 配置会被模块默认参数静默替换——清空请一律走 Reset。
func (cf *CuckooFilter) Reset(ctx context.Context) error {
	err := cf.impl.Reset(ctx)
	if err != nil && IsUnavailable(err) {
		// 不做 FailPolicy 兜底（失败必须可见），但包装为哨兵错误保持可感知
		return fallbackErr(err)
	}
	return err
}

// ---------------------------------------------------------------------------
// 模块版实现：原生 CF.* 命令（依赖 RedisBloom cuckoo 模块）
// ---------------------------------------------------------------------------

type cfCmdImpl struct {
	client *redisClient
	key    string
	cfg    cuckooConfig
	// once 是惰性 CF.RESERVE 闸门（每代只执行一次）。用 atomic.Pointer 包
	// sync.Once 以便 Reset 整键销毁后复位：DEL 之后 Store 一个新 sync.Once，
	// 后续 Add 重新触发 CF.RESERVE，避免 once 燃尽残留导致过滤器被模块默认
	// 参数（capacity=100、bucketSize=2、maxIterations=20）隐式重建、静默
	// 作废 WithCuckooCapacity/WithBucketSize/WithMaxIterations/WithExpansion。
	// 构造点必须显式 Store(new(sync.Once))——atomic.Pointer 零值 Load 返回 nil。
	once atomic.Pointer[sync.Once]
}

// ensureReserve 在配置了容量时对过滤器执行一次 CF.RESERVE 预分配。
// 对已存在的过滤器（CF.RESERVE 报 "item exists"/"already exists"）容错忽略
// （维持武装，闸门视为已消费）。
// 其他错误（网络失败、WRONGTYPE 等）时解除武装：Store 一个新 sync.Once，
// 下次 Add 重试 CF.RESERVE。sync.Once 不辨闭包成败——若不解除武装，一次
// 瞬态网络失败会永久燃尽闸门，过滤器随后被模块默认参数隐式重建，
// With* 配置静默作废（与 Reset 的无条件复位同纪律的两半：一个管销毁、
// 一个管失败）。
func (cf *cfCmdImpl) ensureReserve(ctx context.Context) error {
	if cf.cfg.capacity <= 0 {
		return nil
	}

	var err error
	cf.once.Load().Do(func() {
		opt := &goredis.CFReserveOptions{
			Capacity:      cf.cfg.capacity,
			BucketSize:    cf.cfg.bucketSize,
			MaxIterations: cf.cfg.maxIterations,
			Expansion:     cf.cfg.expansion,
		}
		err = cf.client.CFReserveWithArgs(ctx, cf.key, opt).Err()
		if err != nil && (strings.Contains(err.Error(), "item exists") || strings.Contains(err.Error(), "already exists")) {
			err = nil // 过滤器已存在：视为已初始化
		}
		if err != nil {
			cf.once.Store(new(sync.Once)) // 失败解除武装：本代闸门弃用，下次重试
		}
	})

	return err
}

// Add/Exists/Del 把原始 item 透传给 CF.* 模块命令，由 go-redis writer
// 序列化（与回退版 marshalItem 的字节口径一致，见 marshal.go）。
func (cf *cfCmdImpl) Add(ctx context.Context, item any) (bool, error) {
	if err := cf.ensureReserve(ctx); err != nil {
		return false, err
	}
	return cf.client.CFAdd(ctx, cf.key, item).Result()
}

func (cf *cfCmdImpl) Exists(ctx context.Context, item any) (bool, error) {
	return cf.client.CFExists(ctx, cf.key, item).Result()
}

// ExistsMulti 单条 CF.MEXISTS 批量检查，结果与入参顺序一一对应。
// 只读路径不触发 ensureReserve（与 Exists 现状一致：真机实测
// CF.MEXISTS/CF.EXISTS 对不存在的键宽容返回全 false、无错误，预建
// 无收益）。item 直发不做 marshalItem 预编码校验——与单条 Exists
// 同口径（go-redis writer 对不可序列化类型 panic 属开发者错误；
// 回退版的预校验差异见 hashImpl）。
func (cf *cfCmdImpl) ExistsMulti(ctx context.Context, items ...any) ([]bool, error) {
	res, err := cf.client.CFMExists(ctx, cf.key, items...).Result()
	if err != nil {
		return res, err
	}
	if len(res) != len(items) {
		return nil, fmt.Errorf("redis: CF.MEXISTS 返回 %d 个结果，期望 %d", len(res), len(items))
	}
	return res, nil
}

// Count 直发 CF.COUNT（只读，不触发 ensureReserve）。键不存在时
// RedisBloom 返回 0 而非报错。返回值为出现次数估计，可能因指纹碰撞
// 高估；模块版可取任意值（CF.ADD 多重集语义），见门面 Count godoc。
func (cf *cfCmdImpl) Count(ctx context.Context, item any) (int64, error) {
	return cf.client.CFCount(ctx, cf.key, item).Result()
}

// AddNX 直发 CF.ADDNX：元素已存在则不插入。CF.ADDNX 返回 0/1
// （BoolCmd，无 -1 形态——-1 是 CF.INSERTNX 的返回），true 表示实际
// 插入。写路径先 ensureReserve（与 Add 相同）。
func (cf *cfCmdImpl) AddNX(ctx context.Context, item any) (bool, error) {
	if err := cf.ensureReserve(ctx); err != nil {
		return false, err
	}
	return cf.client.CFAddNX(ctx, cf.key, item).Result()
}

// AddMulti 批量插入走单条 CF.INSERT：options 恒置 nil——不带 CAPACITY/
// NOCREATE，参数预分配统一经 ensureReserve 的 CF.RESERVE 通道（避免
// RESERVE 与 INSERT 双通道配置语义分裂，架构定稿）。结果 1/-1 由
// BoolSliceCmd 归一为 true/false（false=该元素插入失败，桶满/驱逐超限）。
// item 不做 marshalItem 预编码校验、直发 writer 序列化（与单条 Add 同
// 口径；回退版的整体前置校验差异见 hashImpl.AddMulti）。
func (cf *cfCmdImpl) AddMulti(ctx context.Context, items ...any) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if err := cf.ensureReserve(ctx); err != nil {
		return nil, err
	}
	res, err := cf.client.CFInsert(ctx, cf.key, nil, items...).Result()
	if err != nil {
		return res, err
	}
	if len(res) != len(items) {
		return nil, fmt.Errorf("redis: CF.INSERT 返回 %d 个结果，期望 %d", len(res), len(items))
	}
	return res, nil
}

func (cf *cfCmdImpl) Del(ctx context.Context, item any) (bool, error) {
	return cf.client.CFDel(ctx, cf.key, item).Result()
}

func (cf *cfCmdImpl) Info(ctx context.Context) (*CuckooInfo, error) {
	info, err := cf.client.CFInfo(ctx, cf.key).Result()
	if err != nil {
		return nil, err
	}

	return &CuckooInfo{
		Size:          info.Size,
		NumBuckets:    info.NumBuckets,
		NumFilters:    info.NumFilters,
		NumItems:      info.NumItemsInserted,
		NumDeletes:    info.NumItemsDeleted,
		Expansion:     info.ExpansionRate,
		BucketSize:    info.BucketSize,
		MaxIterations: info.MaxIteration,
	}, nil
}

// Reset 删除 CF.* 键并复位惰性 CF.RESERVE 闸门，后续 Add 重新按配置
// CF.RESERVE 重建。闸门复位无条件执行（defer，不区分 DEL 成败）：DEL 实际
// 执行但客户端收到网络错误时，"仅成功才复位"会让旧闸门燃尽残留 → 后续
// Add 用模块默认参数隐式重建、With* 配置静默作废；双 RESERVE 竞态的代价
// 只是 ensureReserve 吞掉的 "item exists"，无害。键不存在时 DEL 返回 0、
// 无错误，天然幂等。
func (cf *cfCmdImpl) Reset(ctx context.Context) error {
	defer cf.once.Store(new(sync.Once))
	return cf.client.Del(ctx, cf.key).Err()
}

// ---------------------------------------------------------------------------
// 无模块回退实现：Hash key + Lua 脚本（不依赖 RedisBloom）
// ---------------------------------------------------------------------------

// 回退版默认参数。
const (
	defaultCuckooCapacity   = 10000
	defaultCuckooBucketSize = 4
	defaultCuckooMaxIter    = 500
)

// hashImpl 是不依赖 RedisBloom 模块的布谷鸟过滤器回退实现。
// 状态存储：单个 Hash key（field = 桶索引十进制字符串，value = 固定长度
// 3×bucketSize 字节的二进制串，每槽 3 字节 = [指纹低字节, 指纹高字节, 方向位]；
// 指纹 0 表示空槽（指纹计算时保证非 0）；方向位 0 表示"本桶是该指纹的 i1"、
// 1 表示"本桶是 i2"。模块版与回退版按能力分派互斥，可共用同一业务 key。
//
// 哈希（Go 侧计算候选桶，Lua 侧计算指纹哈希，公式一致；item 均为
// marshalItem 编码后的规范字节，格式冻结见 marshal.go）：
//   - fp = xxh3.Hash(marshalItem(item)) & 0xFFFF（2 字节指纹，0 时取 1）——**2 字节空间
//     大幅降低指纹冲突**：1 字节（255 种）在元素多时冲突严重，驱逐链无法
//     区分同指纹的不同元素（owner），会把元素指纹移入"另一同指纹元素的
//     候选桶"造成放错（方向错误假阴性）；2 字节（65535 种）冲突率极低，
//     驱逐链的 alternate 恒为该指纹 owner 的候选桶。
//   - i1 = xxh3.Hash(marshalItem(item)) % numBuckets
//   - i2 = (i1 + h(fp)) % numBuckets —— 模加候选桶关系（Lua 5.1 无位运算，
//     XOR 需算术模拟；模加可直接计算）
//   - h(fp) = (fp × 2654435761) % 2^32 % numBuckets —— 乘法哈希（模拟 32 位
//     回绕；fp < 65536 时乘积 < 2^53，Lua double 可精确表示）
//
// ⚠️ v0.7.0 起主哈希由 fnv1a（FNV-1a 64 位）换为 xxh3.Hash（xxh3-64）——
// **BREAKING**：存量 Lua cuckoo 过滤器（本回退实现）的条目按旧哈希定位，
// 升级后对旧 key 的 Exists 会假 miss、Del 失效，必须重建过滤器（Del 旧
// key 后重新灌入，或换新 key）。换哈希动机：① xxh3 吞吐显著高于 FNV-1a；
// ② FNV-1a 低位雪崩质量差，i1 = h % numBuckets 在 numBuckets 为 2 的幂时
// 只用低位、桶分布偏斜，fp = h & 0xFFFF 同样受低位相关性影响，xxh3-64
// 全位雪崩均匀修正该偏斜。i2 的派生（hashFingerprint 乘法哈希）不变。
//
// 驱逐机制（Add Lua 脚本）：两候选桶均满时确定性扰动选一个槽位踢出旧指纹。
// 被踢指纹的 alternate 桶由**方向位**决定：方向 0（本桶是 i1）→ 去
// (cur + h(fp)) % n；方向 1（本桶是 i2）→ 去 (cur - h(fp)) % n，且方向取反。
// 该不变量保证：驱逐链上每个指纹始终位于其两个候选桶之一——**Add 返回 true
// 的元素 Exists 必命中（无假阴性）**；仅当驱逐链超过 maxIterations（桶过载）
// 时该次 Add 返回 false（与 CF.ADD 满时返回 false 的语义对齐），链尾指纹
// 可能被挤出（cuckoo 超载的正常行为：元素被驱逐丢失，非方向错误）。
//
// hashImpl 的全部状态位于该 Hash（桶 field 即全部），无辅助键/辅助
// field/跨请求暂存槽，Reset = DEL 该 Hash 即完整清空。
type hashImpl struct {
	client        *redisClient
	key           string
	bucketSize    int64
	numBuckets    int64
	maxIterations int64
}

func newHashImpl(client *redisClient, key string, cfg cuckooConfig) *hashImpl {
	bucketSize := cfg.bucketSize
	if bucketSize <= 0 {
		bucketSize = defaultCuckooBucketSize
	}
	capacity := cfg.capacity
	if capacity <= 0 {
		capacity = defaultCuckooCapacity
	}
	maxIter := cfg.maxIterations
	if maxIter <= 0 {
		maxIter = defaultCuckooMaxIter
	}

	numBuckets := max(capacity/bucketSize, 1)

	return &hashImpl{
		client:        client,
		key:           key,
		bucketSize:    bucketSize,
		numBuckets:    numBuckets,
		maxIterations: maxIter,
	}
}

// hashFingerprint 指纹哈希：h(fp) = (fp × 2654435761) % 2^32 % numBuckets。
// 与 Lua 脚本中的实现保持一致（乘法哈希 + 32 位回绕模拟；
// fp < 256 时乘积 < 2^53，Lua double 与 Go int64 均精确）。
func (h *hashImpl) hashFingerprint(fp int64) int64 {
	v := (fp * 2654435761) % (1 << 32)
	return v % h.numBuckets
}

// cuckooHashs 计算 item 的指纹与两个候选桶索引（模加候选桶关系）。
// item 先经 marshalItem 编码为规范字节再 xxh3-64（编码格式与 go-redis
// writer 对齐并冻结，见 marshal.go——存量桶数据的有效性依赖该格式不变）；
// 不支持的类型返回数据类错误，调用方不得继续发命令。
func (h *hashImpl) cuckooHashs(item any) (fp int64, i1, i2 int64, err error) {
	data, err := marshalItem(item)
	if err != nil {
		return 0, 0, 0, err
	}
	h1 := xxh3.Hash(data)
	fp = int64(h1 & 0xFFFF) // 2 字节指纹（0 时取 1，避免与空槽哨兵冲突）
	if fp == 0 {
		fp = 1
	}
	i1 = int64(h1 % uint64(h.numBuckets))
	i2 = (i1 + h.hashFingerprint(fp)) % h.numBuckets
	return
}

// cuckooAddScript 原子插入：
//  1. 两候选桶任一已含指纹 → 返回 0（已存在，幂等，对齐 CF.ADD 语义）
//  2. 任一候选桶有空槽 → 插入返回 1（新元素放入 i1，方向位 0）
//  3. 均满 → 确定性扰动选驱逐槽位；被踢指纹按方向位计算 alternate 桶
//     （方向 0 → +h、方向 1 → -h）链式插入，方向取反；最多 maxIterations 次，
//     成功返回 1，超限返回 0。
//
// 槽编码：每槽 3 字节 [指纹低字节, 指纹高字节, 方向位]，桶 value 固定
// 3×bucketSize 字节。
var cuckooAddScript = goredis.NewScript(`
local fp = tonumber(ARGV[1])
local i1 = tonumber(ARGV[2])
local i2 = tonumber(ARGV[3])
local maxIter = tonumber(ARGV[4])
local bucketSize = tonumber(ARGV[5])
local numBuckets = tonumber(ARGV[6])
local key = KEYS[1]

-- 指纹哈希（与 Go 侧 hashFingerprint 一致）
local function hashFp(f)
	local v = (f * 2654435761) % 4294967296
	return v % numBuckets
end

local function readBucket(idx)
	local raw = redis.call('HGET', key, idx)
	if not raw then
		return nil
	end
	local bytes = {string.byte(raw, 1, -1)}
	local slots = {}
	for j = 1, bucketSize do
		local f = bytes[(j-1)*3+1] + bytes[(j-1)*3+2] * 256
		slots[j] = {f, bytes[(j-1)*3+3]}
	end
	return slots
end

local function writeBucket(idx, slots)
	local t = {}
	for j = 1, bucketSize do
		t[(j-1)*3+1] = string.char(slots[j][1] % 256)
		t[(j-1)*3+2] = string.char(math.floor(slots[j][1] / 256))
		t[(j-1)*3+3] = string.char(slots[j][2])
	end
	redis.call('HSET', key, idx, table.concat(t))
end

local function contains(slots, fp)
	if not slots then
		return false
	end
	for j = 1, bucketSize do
		if slots[j][1] == fp then
			return true
		end
	end
	return false
end

-- 已存在检查（幂等，对齐 CF.ADD）
if contains(readBucket(i1), fp) or contains(readBucket(i2), fp) then
	return 0
end

local cur = i1
local curFp = fp
local curDir = 0 -- 新元素放入 i1：本桶是该指纹的 i1（方向 0）
for iter = 1, maxIter do
	local slots = readBucket(cur)
	if not slots then
		-- 桶不存在：以全空槽创建（固定长度 3×bucketSize），首槽放指纹
		local arr = {}
		for j = 1, bucketSize do
			arr[j] = {0, 0}
		end
		arr[1] = {curFp, curDir}
		writeBucket(cur, arr)
		return 1
	end
	-- 找空槽
	local placed = false
	for j = 1, bucketSize do
		if slots[j][1] == 0 then
			slots[j] = {curFp, curDir}
			placed = true
			break
		end
	end
	if placed then
		writeBucket(cur, slots)
		return 1
	end
	-- 桶满：确定性扰动选驱逐槽位
	local victimIdx = (iter * 31 + curFp) % bucketSize + 1
	local victim = slots[victimIdx][1]
	local victimDir = slots[victimIdx][2]
	slots[victimIdx] = {curFp, curDir}
	writeBucket(cur, slots)
	-- 链式：victim 去它的 alternate（方向位决定 +h 或 -h），方向取反
	curFp = victim
	if victimDir == 0 then
		-- 本桶是 victim 的 i1 → alternate = i2 = cur + h
		cur = (cur + hashFp(victim)) % numBuckets
		curDir = 1
	else
		-- 本桶是 victim 的 i2 → alternate = i1 = cur - h
		cur = (cur - hashFp(victim) + numBuckets) % numBuckets
		curDir = 0
	end
end
return 0
`)

func (h *hashImpl) Add(ctx context.Context, item any) (bool, error) {
	fp, i1, i2, err := h.cuckooHashs(item)
	if err != nil {
		return false, err
	}
	n, err := cuckooAddScript.Run(ctx, h.client, []string{h.key},
		fp, i1, i2, h.maxIterations, h.bucketSize, h.numBuckets).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// cuckooExistsScript 原子存在性检查：两个候选桶任一含指纹返回 1。
var cuckooExistsScript = goredis.NewScript(`
local fp = tonumber(ARGV[1])
local i1 = tonumber(ARGV[2])
local i2 = tonumber(ARGV[3])
local bucketSize = tonumber(ARGV[4])
local key = KEYS[1]

local function bucketContains(idx)
	local raw = redis.call('HGET', key, idx)
	if not raw then
		return 0
	end
	local bytes = {string.byte(raw, 1, -1)}
	for j = 1, bucketSize do
		if bytes[(j-1)*3+1] + bytes[(j-1)*3+2] * 256 == fp then
			return 1
		end
	end
	return 0
end

if bucketContains(i1) == 1 then
	return 1
end
return bucketContains(i2)
`)

func (h *hashImpl) Exists(ctx context.Context, item any) (bool, error) {
	fp, i1, i2, err := h.cuckooHashs(item)
	if err != nil {
		return false, err
	}
	n, err := cuckooExistsScript.Run(ctx, h.client, []string{h.key}, fp, i1, i2, h.bucketSize).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ExistsMulti 单条 EVAL 批量检查（cuckooExistsMultiScript），结果与入参
// 顺序一一对应。Go 侧先全量前置编码校验（cuckooHashs）：任一 item 属
// 不支持类型即整体返回数据类错误、**不发命令**（对齐 bloom 惯例，避免
// 半批执行）。cfCmdImpl 路径不做预编码校验（原始 item 直发 writer 序列化）
// ——两路径差异是既定的编码责任边界，见各自注释。
func (h *hashImpl) ExistsMulti(ctx context.Context, items ...any) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	// ARGV = 每 item 的 {fp,i1,i2} 三元组 + 末尾 bucketSize
	args := make([]any, 0, len(items)*3+1)
	for _, it := range items {
		fp, i1, i2, err := h.cuckooHashs(it)
		if err != nil {
			return nil, err
		}
		args = append(args, fp, i1, i2)
	}
	args = append(args, h.bucketSize)

	res, err := cuckooExistsMultiScript.Run(ctx, h.client, []string{h.key}, args...).Int64Slice()
	if err != nil {
		return nil, err
	}
	if len(res) != len(items) {
		return nil, fmt.Errorf("redis: ExistsMulti 回退脚本返回 %d 个结果，期望 %d", len(res), len(items))
	}
	out := make([]bool, len(res))
	for i, v := range res {
		out[i] = v == 1
	}
	return out, nil
}

// Count 统计指纹在两个候选桶中的槽数（cuckooCountScript）。回退版 Add
// 为去重语义，无碰撞时恒 0/1；同指纹碰撞的不同元素会计入同一匹配（高估
// 来源，见门面 Count godoc）。只读，不改变任何状态。
func (h *hashImpl) Count(ctx context.Context, item any) (int64, error) {
	fp, i1, i2, err := h.cuckooHashs(item)
	if err != nil {
		return 0, err
	}
	return cuckooCountScript.Run(ctx, h.client, []string{h.key}, fp, i1, i2, h.bucketSize).Int64()
}

// AddNX 复用 cuckooAddScript、零新脚本：该脚本本就是 NX 语义——任一候选
// 桶已含指纹即返回 0（存在即不加）。回退版 Add 即 NX，AddNX 为其显式别名。
func (h *hashImpl) AddNX(ctx context.Context, item any) (bool, error) {
	return h.Add(ctx, item)
}

// AddMulti 批量插入走单条 EVAL（cuckooExistsMultiScript 的写版
// cuckooAddMultiScript），结果与入参顺序一一对应。Go 侧先全量前置编码
// 校验（cuckooHashs）：任一 item 属不支持类型即整体返回数据类错误、
// **不发命令**（对齐 ExistsMulti/bloom 惯例）。整条 EVAL 在 Redis 侧原子
// ——失败/响应丢失时全批要么已生效要么未生效；回退版去重语义下重试同批
// 也不会重复增值（NX 特性），真正需警惕重试翻倍的是模块版 CF.INSERT
// （多重集），见门面 AddMulti 重试警告。
//
// 成本声明：单条 Lua 时长 O(n×maxIterations×bucketSize)，超大批量阻塞
// Redis，由调用方控批，本库不分块。
func (h *hashImpl) AddMulti(ctx context.Context, items ...any) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	// ARGV = 每 item 的 {fp,i1,i2} 三元组 + 末尾 {maxIterations,bucketSize,numBuckets}
	args := make([]any, 0, len(items)*3+3)
	for _, it := range items {
		fp, i1, i2, err := h.cuckooHashs(it)
		if err != nil {
			return nil, err
		}
		args = append(args, fp, i1, i2)
	}
	args = append(args, h.maxIterations, h.bucketSize, h.numBuckets)

	res, err := cuckooAddMultiScript.Run(ctx, h.client, []string{h.key}, args...).Int64Slice()
	if err != nil {
		return nil, err
	}
	if len(res) != len(items) {
		return nil, fmt.Errorf("redis: AddMulti 回退脚本返回 %d 个结果，期望 %d", len(res), len(items))
	}
	out := make([]bool, len(res))
	for i, v := range res {
		out[i] = v == 1
	}
	return out, nil
}

// cuckooDelScript 原子删除：两候选桶中任一找到指纹即置空槽（指纹与方向位均清 0）并返回 1。
var cuckooDelScript = goredis.NewScript(`
local fp = tonumber(ARGV[1])
local i1 = tonumber(ARGV[2])
local i2 = tonumber(ARGV[3])
local bucketSize = tonumber(ARGV[4])
local key = KEYS[1]

local function removeFrom(idx)
	local raw = redis.call('HGET', key, idx)
	if not raw then
		return 0
	end
	local bytes = {string.byte(raw, 1, -1)}
	for j = 1, bucketSize do
		if bytes[(j-1)*3+1] + bytes[(j-1)*3+2] * 256 == fp then
			bytes[(j-1)*3+1] = 0
			bytes[(j-1)*3+2] = 0
			bytes[(j-1)*3+3] = 0
			local t = {}
			for k = 1, #bytes do
				t[k] = string.char(bytes[k])
			end
			redis.call('HSET', key, idx, table.concat(t))
			return 1
		end
	end
	return 0
end

local r = removeFrom(i1)
if r == 1 then
	return 1
end
return removeFrom(i2)
`)

func (h *hashImpl) Del(ctx context.Context, item any) (bool, error) {
	fp, i1, i2, err := h.cuckooHashs(item)
	if err != nil {
		return false, err
	}
	n, err := cuckooDelScript.Run(ctx, h.client, []string{h.key}, fp, i1, i2, h.bucketSize).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// cuckooInfoScript 统计占用：遍历全部桶，返回 {占用桶数, 元素总数}。
var cuckooInfoScript = goredis.NewScript(`
local key = KEYS[1]
local bucketSize = tonumber(ARGV[1])
local fields = redis.call('HKEYS', key)
local buckets = 0
local total = 0
for _, f in ipairs(fields) do
	buckets = buckets + 1
	local raw = redis.call('HGET', key, f)
	local bytes = {string.byte(raw, 1, -1)}
	for j = 1, bucketSize do
		if bytes[(j-1)*3+1] ~= 0 or bytes[(j-1)*3+2] ~= 0 then
			total = total + 1
		end
	end
end
return {buckets, total}
`)

// cuckooExistsMultiScript 批量存在性检查：ARGV 为每 item 的 {fp,i1,i2}
// 三元组序列，末尾附 bucketSize；KEYS[1] 为 Hash key。逐项复刻
// cuckooExistsScript 的双候选桶指纹检查（脚本内可省短路，逐项独立），
// 按入参顺序返回 n 个 0/1。经 Script.Run 走 EVALSHA 缓存 + NOSCRIPT
// 自动回退。
var cuckooExistsMultiScript = goredis.NewScript(`
local key = KEYS[1]
local bucketSize = tonumber(ARGV[#ARGV])
local out = {}
for idx = 1, #ARGV - 1, 3 do
	local fp = tonumber(ARGV[idx])
	local i1 = tonumber(ARGV[idx + 1])
	local i2 = tonumber(ARGV[idx + 2])
	local found = 0
	for _, bidx in ipairs({i1, i2}) do
		local raw = redis.call('HGET', key, bidx)
		if raw and found == 0 then
			local bytes = {string.byte(raw, 1, -1)}
			for j = 1, bucketSize do
				if bytes[(j-1)*3+1] + bytes[(j-1)*3+2] * 256 == fp then
					found = 1
					break
				end
			end
		end
	end
	out[#out + 1] = found
end
return out
`)

// cuckooCountScript 统计指纹在两个候选桶中的匹配槽数之和（对应 CF.COUNT
// 的"次数估计"语义，独立于 cuckooExistsScript、不复用——exists 是布尔短路
// 口径，count 需逐槽累加）。i1 == i2 时只扫单桶，避免同桶重复计数。
var cuckooCountScript = goredis.NewScript(`
local fp = tonumber(ARGV[1])
local i1 = tonumber(ARGV[2])
local i2 = tonumber(ARGV[3])
local bucketSize = tonumber(ARGV[4])
local key = KEYS[1]

local function countIn(idx)
	local raw = redis.call('HGET', key, idx)
	if not raw then
		return 0
	end
	local bytes = {string.byte(raw, 1, -1)}
	local c = 0
	for j = 1, bucketSize do
		if bytes[(j-1)*3+1] + bytes[(j-1)*3+2] * 256 == fp then
			c = c + 1
		end
	end
	return c
end

if i1 == i2 then
	return countIn(i1)
end
return countIn(i1) + countIn(i2)
`)

// cuckooAddMultiScript 回退版批量写入：ARGV = 每 item 的 {fp,i1,i2} 三元组
// 序列 + 末尾 {maxIterations,bucketSize,numBuckets}，按入参顺序逐项返回
// 1/0（1=实际插入，0=已存在或驱逐超限，两因不可区分——与单条 Add 口径一致）。
//
// ⚠️ 漂移防线：insertOne 函数与单条 cuckooAddScript 的脚本体**逻辑同源**
// （已存在检查 → 空槽直放 → 满桶按方向位驱逐链式置换），改一处必须同步
// 另一处；回归防线见 cuckoo_hash_test.go 的
// TestCuckooHashAddMultiConsistency（批量与逐条 Add 逐一相等锚定）。
// 成本：单条 EVAL 内 O(n×maxIterations×bucketSize)，超大批量阻塞 Redis，
// 由调用方控批。
var cuckooAddMultiScript = goredis.NewScript(`
local key = KEYS[1]
local maxIter = tonumber(ARGV[#ARGV - 2])
local bucketSize = tonumber(ARGV[#ARGV - 1])
local numBuckets = tonumber(ARGV[#ARGV])

-- 指纹哈希（与 Go 侧 hashFingerprint 一致）
local function hashFp(f)
	local v = (f * 2654435761) % 4294967296
	return v % numBuckets
end

local function readBucket(idx)
	local raw = redis.call('HGET', key, idx)
	if not raw then
		return nil
	end
	local bytes = {string.byte(raw, 1, -1)}
	local slots = {}
	for j = 1, bucketSize do
		local f = bytes[(j-1)*3+1] + bytes[(j-1)*3+2] * 256
		slots[j] = {f, bytes[(j-1)*3+3]}
	end
	return slots
end

local function writeBucket(idx, slots)
	local t = {}
	for j = 1, bucketSize do
		t[(j-1)*3+1] = string.char(slots[j][1] % 256)
		t[(j-1)*3+2] = string.char(math.floor(slots[j][1] / 256))
		t[(j-1)*3+3] = string.char(slots[j][2])
	end
	redis.call('HSET', key, idx, table.concat(t))
end

local function contains(slots, fp)
	if not slots then
		return false
	end
	for j = 1, bucketSize do
		if slots[j][1] == fp then
			return true
		end
	end
	return false
end

-- insertOne 复刻 cuckooAddScript 全体（同源，改动须双向同步）
local function insertOne(fp, i1, i2)
	-- 已存在检查（幂等，对齐 CF.ADD）
	if contains(readBucket(i1), fp) or contains(readBucket(i2), fp) then
		return 0
	end

	local cur = i1
	local curFp = fp
	local curDir = 0 -- 新元素放入 i1：本桶是该指纹的 i1（方向 0）
	for iter = 1, maxIter do
		local slots = readBucket(cur)
		if not slots then
			-- 桶不存在：以全空槽创建（固定长度 3×bucketSize），首槽放指纹
			local arr = {}
			for j = 1, bucketSize do
				arr[j] = {0, 0}
			end
			arr[1] = {curFp, curDir}
			writeBucket(cur, arr)
			return 1
		end
		-- 找空槽
		local placed = false
		for j = 1, bucketSize do
			if slots[j][1] == 0 then
				slots[j] = {curFp, curDir}
				placed = true
				break
			end
		end
		if placed then
			writeBucket(cur, slots)
			return 1
		end
		-- 桶满：确定性扰动选驱逐槽位
		local victimIdx = (iter * 31 + curFp) % bucketSize + 1
		local victim = slots[victimIdx][1]
		local victimDir = slots[victimIdx][2]
		slots[victimIdx] = {curFp, curDir}
		writeBucket(cur, slots)
		-- 链式：victim 去它的 alternate（方向位决定 +h 或 -h），方向取反
		curFp = victim
		if victimDir == 0 then
			-- 本桶是 victim 的 i1 → alternate = i2 = cur + h
			cur = (cur + hashFp(victim)) % numBuckets
			curDir = 1
		else
			-- 本桶是 victim 的 i2 → alternate = i1 = cur - h
			cur = (cur - hashFp(victim) + numBuckets) % numBuckets
			curDir = 0
		end
	end
	return 0
end

local out = {}
for idx = 1, #ARGV - 3, 3 do
	out[#out + 1] = insertOne(tonumber(ARGV[idx]), tonumber(ARGV[idx + 1]), tonumber(ARGV[idx + 2]))
end
return out
`)

// Info 现场遍历桶统计占用。回退版仅 Size/NumBuckets/NumItems/BucketSize
// 四字段有效，其余（NumFilters/NumDeletes/Expansion/MaxIterations）恒 0。
func (h *hashImpl) Info(ctx context.Context) (*CuckooInfo, error) {
	res, err := cuckooInfoScript.Run(ctx, h.client, []string{h.key}, h.bucketSize).Int64Slice()
	if err != nil {
		return nil, err
	}

	var buckets, items int64
	if len(res) > 0 {
		buckets = int64(res[0])
	}
	if len(res) > 1 {
		items = int64(res[1])
	}

	return &CuckooInfo{
		Size:       buckets * h.bucketSize, // 估算：占用桶 × 桶字节数
		NumBuckets: buckets,
		NumItems:   items,
		BucketSize: h.bucketSize,
	}, nil
}

// Reset 删除整个 Hash key 即完成清空：hashImpl 的全部状态位于该 Hash
// （field = 桶索引、value = 指纹字节），无辅助 field、无 victim 暂存槽
// （驱逐在单条 Lua 内完成）、结构体字段均为构造期冻结的不可变配置，
// 因此不需要任何复位胶水。键不存在时 DEL 返回 0、无错误，天然幂等。
func (h *hashImpl) Reset(ctx context.Context) error {
	return h.client.Del(ctx, h.key).Err()
}
