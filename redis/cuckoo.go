package redis

import (
	"context"
	"strings"
	"sync"
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

// cuckooFilterImpl 是布谷鸟过滤器的实现接口：模块版（cfCmdImpl，见
// cuckoo_cf.go）与无模块回退版（hashImpl，见 cuckoo_hash.go）各自实现，
// 由 NewCuckooFilter 按能力分派。
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
//   - 服务器加载了 RedisBloom 的 cuckoo 模块 → cfCmdImpl（原生 CF.* 命令，见 cuckoo_cf.go）
//   - 未加载 → hashImpl（Hash + Lua 回退，普通 Redis 即可运行，无模块依赖，见 cuckoo_hash.go）
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
