package redis

import (
	"context"
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
	prefillConfig       // 预填充门控配置（fn==nil 时未启用，工厂不建 coord，同 bloomConfig 方式）
	capacity      int64 // 预估容量（模块版首写前恒 CF.RESERVE 预建，未显式传参按默认 1000000；回退版决定桶数量 numBuckets=capacity/bucketSize，默认 10000 为存量键布局兼容约束）
	maxIterations int64 // 最大踢出迭代次数
	bucketSize    int64 // 桶大小
	expansion     int64 // 扩容因子（仅模块版 CF.RESERVE 使用）
}

// CuckooConfig 是 CuckooFilter 的配置类型别名，供 WithFailPolicy 泛型参数使用。
type CuckooConfig = cuckooConfig

func defaultCuckooConfig() cuckooConfig {
	return cuckooConfig{prefillConfig: defaultPrefillConfig()}
}

// WithCuckooCapacity 设置预估容量（命名避免与 BloomFilter 的 WithCapacity
// 冲突——两者是不同类型 Option，Go 同包不允许同名重载）。
// 非法值（n<=0）静默忽略，等同未显式传参（模块版按默认容量 1000000 预建；
// 回退版按默认 10000 决定桶数量）。
// 模块版：决定 CF.RESERVE 预建容量；回退版：容量决定桶数量
// （numBuckets = capacity / bucketSize）。
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

// WithCuckooPrefill 声明式启用布谷鸟过滤器预填充门控（使用形态与
// WithPrefill 一致）：新建/重置后由库负责预填充状态管理（Redis 权威状态
// 键 {base}:__prefill）、抢占重建、指数退避与多实例降级同步。fn==nil
// 静默忽略（=不启用，对齐 WithPrefill 惯例）；opts 作用到
// &c.prefillConfig（WithRebuildTimeout/WithRetryBackoff/WithSyncInterval），
// 非法值静默忽略。
//
// PrefillFunc 回灌契约（同 WithPrefill）：
//   - 必须幂等可重试：库可能因退避/抢占在清空后再次调用；
//   - 以数据源为扫描基准：清空窗口内的增量写入由回灌以数据源为准补回
//     （Add/AddNX/AddMulti/Del 非 Ready 期照常放行真实写入，Del 亦同）；
//   - 应响应 ctx 取消：预算 RebuildTimeout，超时按失败计并指数退避；
//   - panic 由库 recover 转 error 走 fail 路径（不崩溃进程）。
//
// 降级语义摘要（本地非新鲜 Ready 时，详见各方法 godoc）：
//   - Exists 恒 (true,nil)、ExistsMulti 非空恒全 true、Count 恒 (1,nil)
//     ——这是状态未就绪期的业务规则，**不受 FailPolicy 影响**（FailPolicy
//     只作用于 Ready 态原路径与写入口原路径的 IsUnavailable 兜底）；
//   - Add/AddNX/AddMulti/Del 放行（非 Ready 也走原路径，真实写入含
//     FailPolicy 分叉）；Info 恒透传不降级；
//   - Reset 被拦截为 force 重建全流程（手工触发重建的唯一入口）；
//     State 查询权威相位；未启用时 State 返回 ErrPrefillDisabled。
func WithCuckooPrefill(fn PrefillFunc, opts ...PrefillOption) CuckooOption {
	return func(c *cuckooConfig) {
		if fn == nil {
			return // 静默忽略 = 不启用
		}
		c.enabled = true
		c.fn = fn
		for _, o := range opts {
			o(&c.prefillConfig)
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
	// Reset 清空整个过滤器（DEL 物理键）并立即按当前配置同步重建（模块版）；
	// 回退版纯 DEL。
	Reset(ctx context.Context) error
}

// CuckooFilter 是布谷鸟过滤器的统一门面，按服务器能力自动分派实现：
//   - 服务器加载了 RedisBloom 的 cuckoo 模块 → cfCmdImpl（原生 CF.* 命令，见 cuckoo_cf.go）
//   - 未加载 → hashImpl（Hash + Lua 回退，普通 Redis 即可运行，无模块依赖，见 cuckoo_hash.go）
//
// 与 BloomFilter 不同，布谷鸟过滤器支持删除（Del），且误判率更低。
//
// 本库不封装 CF.INSERT/CF.INSERTNX：CF.* 路径构造即连接——工厂与 Reset
// 都同步执行 CF.RESERVE（未显式传容量按默认 1000000；失败返回错误、
// 不交付实例/不留下半重建）；回退版 Hash 键惰性、空即就绪，无建立动作，
// 仅在构造期做类型校验。
//
// 两路径未显式传参时的有效容量口径不同（有意为之，非缺陷）：模块版恒按
// 1000000 建立，这是分配契约——键在构造时即以该容量在服务端建立；回退版
// 不存在建立动作，其默认容量 10000 仅决定寻址布局（numBuckets），不是
// 分配量，且是存量键的兼容冻结约束。对容量敏感的应用请一律显式
// WithCuckooCapacity，两侧取值一致即可。
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
	policy FailPolicy          // 失效兜底策略（默认 FailOpen）
	coord  *prefillCoordinator // 预填充协调器（nil=未启用 WithCuckooPrefill）
}

// NewCuckooFilter 创建布谷鸟过滤器（挂 *redisClient）。
// 分派逻辑：按 Capability 缓存选择实现（Capability().HasCuckoo()，与
// bloom.go 的 NewBloomFilter 分派方式一致）；构造前未显式
// Capability().Probe(ctx) 时按保守态分派（恒回退 Hash+Lua 路径）。
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
//
// 构造即连接：CF.* 路径同步执行 CF.RESERVE（未显式传容量按默认 1000000）
// ——键不存在即建立、既有真 CF 键复用并按声明的布局口径校验（bucketSize/
// maxIterations 等值、capacity 容纳判定；expansion 不回读比对）；回退版
// 校验键类型为 none/hash（不建键——空 Hash 与不存在对全部命令等价，占位
// 写入会污染 Info 统计）。类型冲突或布局不符在构造期 fail-loud（数据类
// 错误、键未被修改）；服务不可用包 ErrRedisUnavailable 哨兵。失败返回
// (nil, err)，不交付半初始化实例；构造失败不随 FailPolicy 兜底。
func (rdb *redisClient) NewCuckooFilter(ctx context.Context, key string, opts ...CuckooOption) (*CuckooFilter, error) {
	cfg := defaultCuckooConfig()
	cfg.policy = FailOpen // 过滤器默认 FailOpen：宁可放行不阻塞业务
	for _, o := range opts {
		o(&cfg)
	}
	// 预填充协调器：启用且 fn!=nil 才创建（impl 天然满足 prefillInner）；
	// 未启用 coord==nil，数据面/Reset/Close 走原路径一字不改。
	mkCoord := func(impl prefillInner) *prefillCoordinator {
		if cfg.fn == nil {
			return nil
		}
		return newPrefillCoordinator(rdb, impl, key, cfg.prefillConfig)
	}

	if rdb.cap.HasCuckoo() {
		impl := &cfCmdImpl{client: rdb, key: key, cfg: cfg}
		if err := impl.connectAll(ctx); err != nil {
			return nil, err
		}
		return &CuckooFilter{impl: impl, policy: cfg.policy, coord: mkCoord(impl)}, nil
	}
	impl := newHashImpl(rdb, key, cfg)
	if err := impl.connect(ctx); err != nil {
		return nil, err
	}
	return &CuckooFilter{impl: impl, policy: cfg.policy, coord: mkCoord(impl)}, nil
}

// prefillProbe 热路径惰性触发：coord 存在时经 triggerLazy 起后台
// force=0 重建（内部按本地相位/CAS 短路，恒调用安全；triggerLazy 以
// WithoutCancel 派生 run ctx——剥离取消与超时、保留调用方 ctx values，
// 与 bloom maybeTrigger(ctx) 口径一致），零 RTT 不阻塞当前请求；
// coord==nil（未启用）为 no-op。数据面入口统一传入调用方 ctx 调用，
// 保证冷启动首请求即能惰性起重建且 fn 的 ctx 可读到调用方 values。
func (cf *CuckooFilter) prefillProbe(ctx context.Context) {
	if cf.coord != nil {
		cf.coord.triggerLazy(ctx)
	}
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
// 启用 WithCuckooPrefill 时非 Ready 也照常放行真实写入（清空窗口内的
// 增量写入由回灌以数据源为准补回）。
// IsUnavailable 时按 FailPolicy 兜底且返回 ErrRedisUnavailable 哨兵
// （errors.Is 可感知）；数据类错误恒原样返回。
func (cf *CuckooFilter) Add(ctx context.Context, item any) (bool, error) {
	cf.prefillProbe(ctx)
	added, err := cf.impl.Add(ctx, item)
	if err != nil && IsUnavailable(err) {
		return cf.fallbackBool(err)
	}
	return added, err
}

// Exists 检查元素是否可能存在于过滤器（布谷鸟过滤器无假阴性，可能有假阳性）。
// 启用 WithCuckooPrefill 且本地非新鲜 Ready 时恒返回 (true, nil)——状态
// 未就绪期"恒存在"是业务规则，不经 FailPolicy（先 prefillProbe 惰性探测
// 一次）；Ready 透传原路径真实查询。
// Redis 服务失效时按兜底策略：FailOpen → (true, nil)（视为存在，防穿透失效
// 但放行业务）；FailClosed → (false, nil)。
// item 属不支持类型时返回数据类错误，不触发兜底。
func (cf *CuckooFilter) Exists(ctx context.Context, item any) (bool, error) {
	if cf.coord != nil && !cf.coord.readyFresh() {
		cf.prefillProbe(ctx)
		return true, nil
	}
	exists, err := cf.impl.Exists(ctx, item)
	if err != nil && IsUnavailable(err) {
		return cf.fallbackBool(err)
	}
	return exists, err
}

// ExistsMulti 批量检查多个元素是否存在，结果与入参顺序一一对应；
// 存在性语义同 Exists（无假阴性，可能假阳性）。单命令/单脚本往返。
// items 为空时返回 (nil, nil)（空入参先于降级判断走原路径，两态一致）。
// 启用 WithCuckooPrefill 且本地非新鲜 Ready 时非空入参恒返回
// 长度=入参数的全 true + nil（业务规则，不经 FailPolicy，先 prefillProbe）。
// IsUnavailable 时整体按 FailPolicy 兜底（FailOpen 全 true / FailClosed
// 全 false）且返回 ErrRedisUnavailable 哨兵（errors.Is 可感知），禁止
// 部分真实部分兜底的混合结果；数据类错误恒原样返回。
func (cf *CuckooFilter) ExistsMulti(ctx context.Context, items ...any) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if cf.coord != nil && !cf.coord.readyFresh() {
		cf.prefillProbe(ctx)
		out := make([]bool, len(items))
		for i := range out {
			out[i] = true
		}
		return out, nil
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
// 启用 WithCuckooPrefill 且本地非新鲜 Ready 时恒返回 (1, nil)：降级期
// 精确计数无意义，且 Count==0 在原语义是"确定不存在"的假阴性结论，
// 未就绪期禁止返回 0（先 prefillProbe）。
// IsUnavailable 时返回 (0, ErrRedisUnavailable 哨兵)（errors.Is 可感知），
// 观测类不随 FailPolicy 分叉；数据类错误恒原样返回。
func (cf *CuckooFilter) Count(ctx context.Context, item any) (int64, error) {
	if cf.coord != nil && !cf.coord.readyFresh() {
		cf.prefillProbe(ctx)
		return 1, nil
	}
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
// 启用 WithCuckooPrefill 时非 Ready 也照常放行真实写入（清空窗口内的
// 增量写入由回灌以数据源为准补回）。
// IsUnavailable 时按 FailPolicy 兜底且返回 ErrRedisUnavailable 哨兵
// （errors.Is 可感知）；数据类错误恒原样返回。
func (cf *CuckooFilter) AddNX(ctx context.Context, item any) (bool, error) {
	cf.prefillProbe(ctx)
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
// 启用 WithCuckooPrefill 时非 Ready 也照常放行真实写入（清空窗口内的
// 增量写入由回灌以数据源为准补回）。
func (cf *CuckooFilter) AddMulti(ctx context.Context, items ...any) ([]bool, error) {
	cf.prefillProbe(ctx)
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
// 启用 WithCuckooPrefill 时非 Ready 也照常放行真实删除（清空窗口内的
// Del 亦由回灌以数据源为准补回）。
// item 属不支持类型时返回数据类错误，不触发兜底。
func (cf *CuckooFilter) Del(ctx context.Context, item any) (bool, error) {
	cf.prefillProbe(ctx)
	deleted, err := cf.impl.Del(ctx, item)
	if err != nil && isNotFoundByText(err) {
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
// 观测类恒透传原路径：启用 WithCuckooPrefill 也不降级、不触发重建。
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
// 模块版注意：Reset 删除后立即按当前配置（未显式传参按默认容量 1000000）
// 同步 CF.RESERVE 重建，返回成功即键已就绪；绕开本方法手工 DEL 后复用
// 实例不自愈（写路径无预建动作）——键生命周期由库管辖，清空一律走 Reset。
//
// 启用 WithCuckooPrefill 后本方法被拦截：手工触发预填充重建的唯一入口，
// 等价 force=1 抢占式全流程——抢占成功 → 等待 Δ（2×syncInterval）→
// 清空重建 → 调用 PrefillFunc 回灌（预算 RebuildTimeout）→ 成功置
// ready、失败置 fail（指数退避），同步执行直至返回。抢占失败（权威状态
// 为 Building）立即返回 ErrRebuildInProgress，不排队不等锁；启用后错误
// 原样返回（ErrRebuildInProgress / ErrRebuildTimeout / cause），不经
// fallbackErr 哨兵包装。未启用保持上述原语义（coord==nil 走原路径）。
func (cf *CuckooFilter) Reset(ctx context.Context) error {
	if cf.coord != nil {
		return cf.coord.run(ctx, 1)
	}
	err := cf.impl.Reset(ctx)
	if err != nil && IsUnavailable(err) {
		// 不做 FailPolicy 兜底（失败必须可见），但包装为哨兵错误保持可感知
		return fallbackErr(err)
	}
	return err
}

// State 直接 GET 权威状态键查询预填充相位（1 RTT，不经本地缓存）。
// 键缺失返回 PrefillUninitialized；错误原样返回（phase 取
// PrefillUninitialized）。未启用预填充返回
// (PrefillUninitialized, ErrPrefillDisabled)。
func (cf *CuckooFilter) State(ctx context.Context) (PrefillPhase, error) {
	if cf.coord == nil {
		return PrefillUninitialized, ErrPrefillDisabled
	}
	return cf.coord.readState(ctx)
}

// Close 释放预填充协调器（停后台同步 ticker 并取消在飞 run），幂等；
// coord==nil（未启用）返回 nil。不进 cuckooFilterImpl 接口，供单独释放
// 门面持有的后台资源（亦经 redisClient.GracefulClose 级联）。
func (cf *CuckooFilter) Close() error {
	if cf.coord != nil {
		cf.coord.Close()
	}
	return nil
}
