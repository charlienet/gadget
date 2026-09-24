// Package redis 封装 go-redis 客户端，提供服务能力探测（版本/模块，见
// Capability）与一批基于 Redis 的数据结构组件：布隆过滤器、布谷鸟过滤器、
// 限流器（令牌桶/漏桶/AtMost）、延迟队列，并统一处理键前缀、熔断、失效
// 兜底策略。所有命令入口以 context.Context 为首参。
package redis

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// BloomFilter 公共面：接口定义、配置与 Option、工厂入口，
// 以及两实现共享的容量数学（bloomBitCount/bloomHashCount）。
// BF.* 命令实现见 bloom_bf.go；bitmap/Lua 实现见 bloom_bitmap.go；
// 集群分片路由共享层见 bloom_shard.go。

// BloomFilter 是 Redis 支撑的布隆过滤器：服务器加载 RedisBloom 模块时
// 使用原生 BF.* 命令，否则回退到 bitmap（GETBIT/SETBIT + Lua）实现，
// 路径分派自动进行；需要模块实现的部署须在构造前显式
// Capability().Probe(ctx)（查询为纯内存读，未探测时按保守态回退 bitmap）。
//
// 容量契约（bitmap 路径）：容量在创建时固定，位图不会扩容。插入量超过
// 预估容量后误判率单调恶化且不可恢复（布隆无删除语义），属应用端容量
// 规划责任；对策为预留充足容量、周期性重建（换新 key，或 Reset 就地清空
// 复用同一实例），或部署 RedisBloom 模块（BF.* 路径支持自动扩容）。
// Reset 在集群分片下非原子（见 Reset 方法注释）。本库不封装 BF.INSERT：
// 工厂 NewBloomFilter 构造即连接——BF.* 对每个物理键执行 BF.RESERVE、
// bitmap 对每个物理键以 SETBIT 末位一次性全额分配 ⌈m/8⌉ 字节（经服务端
// 原子脚本建键并校验布局），构造失败不交付实例；对已存在键仅校验复用、
// 不改写其参数（BF.* 的 falsePositive 无法服务端核验，见工厂 godoc），
// 布局不符（bitmap 长度 mismatch）报数据类错误，Reset 或换键解决。
// 键生命周期由库管辖：绕开 Reset 手工 DEL 后复用实例不自愈。
//
// 集群分片（Redis Cluster）：默认关闭。经 NewBloomFilter 显式组合
// WithShardCount(n>1)、且 Mode()==ModeCluster 时，把过滤器打散为多个
// <base>#<idx> 物理键（见 bloom_shard.go、WithShardCount）；standalone/
// sentinel/ring、或集群未显式开启（默认 n=1，含不带该 Option 的
// NewBloomFilterWithEstimate）时不分片、键名与行为完全不变。WithCapacity
// 的容量在分片下是全局量，均摊到每分片。分片后实测误判率略高于设定值
// （各分片负载天然不均），对 FPR 敏感的场景请预留余量。
//
// 开启分片与未分片（含 standalone、集群默认）之间物理键名不同（前者带
// #idx 后缀），切换部署形态或分片配置时旧键空间不会被读到，等同于重建
// 过滤器。集群各节点须配置同构：模块/Lua 能力探测只命中单节点，配置不
// 一致时可能分派错路径。
//
// item 序列化承诺：item 为 any，位哈希与分片路由前统一经 marshalItem
// 编码为规范字节（见 marshal.go）。编码格式与 go-redis v9.22
// internal/proto/writer.go 的 WriteArg 逐类型对齐并**冻结**（int/uint 十
// 进制文本、float 'f' 最短表示、bool→"1"/"0"、time.Time→RFC3339Nano、
// net.IP 原始字节、支持 encoding.BinaryMarshaler；不支持指针变体）——
// 存量过滤器数据的有效性依赖该格式永久不变，格式变更属 breaking change。
// 不支持的类型返回数据类错误（不 panic、不触发 FailPolicy 兜底、不发
// 命令）。同一 item 在 BF.*（服务端序列化）与 bitmap（客户端 marshalItem
// 后哈希）两条路径、以及路由与位哈希之间字节口径一致。
type BloomFilter interface {
	// Add adds an item to the filter. 返回 true 表示调用前该 item 不可能
	// 存在（对齐 RedisBloom BF.ADD 语义），false 表示它可能已存在。
	// Returns true if the item could not have existed before this call.
	Add(ctx context.Context, item any) (bool, error)

	// Exists checks whether an item has possibly been added to the filter.
	// Returns false if definitely not present; true if it may be present.
	Exists(ctx context.Context, item any) (bool, error)

	// AddMulti adds multiple items at once. Returns a slice of booleans
	// indicating whether each item was newly added, in the same order as
	// items. 集群分片下按分片分组批量提交。中途失败时可能已部分写入，
	// 整体重试安全（置位幂等），但重复条目的返回值语义失准。
	//
	// 失效兜底（集群分片）：任一分片组不可用即整体按 FailPolicy 兜底
	// （FailOpen 全 true / FailClosed 全 false）并返回 ErrRedisUnavailable，
	// 不产生"部分真实部分兜底"的混合结果。
	//
	// 任一 item 属不支持类型（marshalItem 失败）时整体返回数据类错误、
	// 不发命令。
	AddMulti(ctx context.Context, items ...any) ([]bool, error)

	// ExistsMulti checks multiple items at once. 结果与入参顺序一一对应；
	// 失效兜底规则与 AddMulti 相同；不支持类型 item 同样整体报错不发命令。
	ExistsMulti(ctx context.Context, items ...any) ([]bool, error)

	// Info returns metadata about the Bloom filter. 聚合所有分片键的
	// 元数据（Size/Capacity/NumItems 为求和）。
	Info(ctx context.Context) (*BloomInfo, error)

	// Card 返回过滤器中不同元素数量的估计值（去重基数，对齐 BF.CARD 语义）。
	//
	// 与 Info().NumItems 的区别：NumItems 是插入口径（BF.* 路径含重复插入
	// 计数），Card 是去重口径。返回值是近似估计，两路径估计器不同、误差无
	// 统一上界承诺，仅作观测，勿做精确业务计数。键不存在返回 0。分片模式
	// 为全分片求和。
	//
	// 误差声明：bitmap 路径由置位数反推（与 Info.NumItems 同源估计量），
	// 位图饱和时钳制到容量上界（估计失效）；BF.* 路径为模块内概率基数
	// 计数器。
	//
	// 成本：与 Info 同级重命令（分片 ×N 往返 / BITCOUNT 全量扫描），勿入
	// 热路径。
	//
	// 服务不可用时返回 (0, ErrRedisUnavailable 哨兵)（errors.Is 可感知），
	// 不随 FailPolicy 分叉——观测类方法无"放行"概念。
	Card(ctx context.Context) (int64, error)

	// Reset 清空过滤器的全部物理键并立即按当前配置同步重建（BF.* 逐键
	// BF.RESERVE；bitmap 逐键 SETBIT 预热全额分配），返回成功即键已就绪。
	// 失败时如实返回（键可能处于已清空未重建态）——重试 Reset 幂等
	// （DEL 与重建皆幂等）。
	//
	// 语义与限制：
	//   - 单键形态（未分片）：DEL <base>，Redis 单命令原子。
	//   - 集群分片：逐分片 DEL <base>#<idx>——跨 slot 无法原子，返回错误时
	//     可能只清空部分分片；DEL 幂等，可安全重试直至成功。
	//   - 幂等：分片键不存在时 DEL 返回 0、无错误，天然幂等。
	//   - 并发：与 Add/Exists 无全序保证，读者可能观察到旧存在性或中间态；
	//     需要强一致清空的场景请改用全新键 + 指针原子替换。
	//   - 失败恒返回错误（errors.Is(ErrRedisUnavailable) 可感知），
	//     与 FailPolicy 取值无关。
	//   - Reset 删除整个键——勿与布隆过滤器共用键。
	//
	// 启用预填充（WithPrefill）后本方法被拦截：手工触发预填充重建的唯一
	// 入口，等价 force=1 抢占式全流程——抢占成功 → 等待 Δ（2×syncInterval）
	// → 清空重建 → 调用 PrefillFunc 回灌（预算 RebuildTimeout）→ 成功置
	// ready、失败置 fail（指数退避），同步执行直至返回。Ready/Failed/
	// Uninitialized 均可被 force 抢占（Failed 同时归零连续失败计数）；
	// 抢占失败（权威状态为 Building）立即返回 ErrRebuildInProgress，不排队
	// 不等锁，重试即可。启用后错误原样返回（ErrRebuildInProgress /
	// ErrRebuildTimeout / cause），不经 fallbackErr 哨兵包装。
	// 见 bloom_prefill_filter.go；未启用保持上述原语义。
	Reset(ctx context.Context) error

	// State 直接 GET 权威状态键查询预填充相位（1 RTT，不经本地缓存）。
	// 键缺失返回 PrefillUninitialized；错误原样返回（phase 取
	// PrefillUninitialized）。未启用预填充返回
	// (PrefillUninitialized, ErrPrefillDisabled)。
	// State 返回错误可作 Redis 可用性探针（1 RTT 权威）；数据面
	// Exists/Count 的降级值（恒 true/恒 1）不携带错误，勿以数据面
	// 错误判断 Redis 健康。
	//
	// 启用预填充后，后台每拍对本实例权威 Ready 相位搭载完整性校验
	// （键存在性/布局，判据/覆盖/盲区/成本见 prefillInner.IntegrityProbe
	// godoc）；检出即本地降级并异步 force=1 重建。已知接受行为
	// （ISSUE-107③）：重建抢占若传输失败，下拍 GET ready → 相位复活
	// → 再检 → 再试，每拍至多 1 个校验 RTT 的透传窗，无害自愈。
	State(ctx context.Context) (PrefillPhase, error)

	// Phase 零 RTT 纯内存读取本地相位快照（非权威），与 State(ctx) 的
	// 对照是设计意图：本方法不发任何 Redis 命令，故不带 ctx（无取消/
	// 超时可传导，诚实签名）。分工线——Phase 用于放行分流（fail-safe
	// 方向，Fresh=false 按降级理解），State(ctx) 权威读用于不可逆决策；
	// 多实例滞后与字段口径见 PhaseInfo godoc。
	//
	// LastSyncErr 仅代表后台 syncOnce 同步路径的底层错误（原样存储不
	// 包装）：Ready 透传态下 Exists/ExistsMulti 的真实错误仍原样返回
	// 调用方，两个观测点互不干扰。可用性指路：对 LastSyncErr 用
	// IsUnavailable/IsNotFound 分类（勿以数据面降级值判断 Redis 健康，
	// 同 State 探针声明）。
	//
	// 未启用预填充返回 (PhaseInfo{}, ErrPrefillDisabled)。
	//
	// 本地视图搭载完整性校验效果（G1）：后台检出键缺失/异类型占用/
	// 布局不符即写降级相位（LastSyncErr 仍只承载传输错误，二者观测点
	// 独立）；组合对不可逆决策仍有 ≤syncInterval 假窗口（权威未变而
	// 本地先降级，或反之，见 State godoc ISSUE-107③ 声明）——不可逆
	// 决策以 State(ctx) 权威读为准的分工线不因校验改变。
	Phase() (PhaseInfo, error)
}

// BloomInfo contains metadata about a Bloom filter.
// bitmap 回退版仅 Capacity/Size/NumItems 有效，NumFilters/Expansion
// 恒 0（不适用）。
type BloomInfo struct {
	Capacity   int64  // configured capacity
	Size       int64  // memory size (bytes)
	NumFilters int64  // number of filters (仅 BF.* 路径有效，bitmap 回退版恒 0)
	NumItems   int64  // approximate number of items
	Expansion  int64  // expansion factor (仅 BF.* 路径有效，bitmap 回退版恒 0)
	Path       string // 实际服务的实现路径（PathBF/PathBitmap，观测实例分派结果；服务不可用兜底返回的空结构体为 ""）
}

// UnimplementedBloomFilter 提供 BloomFilter 预填充扩展方法（State/Phase）
// 的默认空实现：返回 (PrefillUninitialized/PhaseInfo{}, ErrPrefillDisabled)
// ——未启用预填充时的正确语义。
// 供既有 BloomFilter 实现方嵌入以平滑对接接口扩展（其余数据面方法仍由
// 外层类型自行实现）：
//
//	type myFilter struct{ ... }
//	func (f *myFilter) Add(...)  { ... } // 既有方法…
//	var _ BloomFilter = (*myFilter)(nil) // 嵌入 UnimplementedBloomFilter 后满足接口
type UnimplementedBloomFilter struct{}

// State 未启用预填充的默认语义。
func (UnimplementedBloomFilter) State(context.Context) (PrefillPhase, error) {
	return PrefillUninitialized, ErrPrefillDisabled
}

// Phase 未启用预填充的默认语义（零 RTT；无本地快照可读）。
func (UnimplementedBloomFilter) Phase() (PhaseInfo, error) {
	return PhaseInfo{}, ErrPrefillDisabled
}

// --- 模块期望声明与路径观测（v0.11.0 G4） ---

// 实现路径观测常量：BloomInfo.Path 的取值，标识实例实际落在哪条实现
// 路径上（Info/Card 等非热路径自检观测用，勿入热路径做逻辑分支）；
// cuckoo 侧对称常量 PathCF/PathHash 见 cuckoo.go。
const (
	PathBF     = "bf"     // BF.* 原生模块路径
	PathBitmap = "bitmap" // bitmap + Lua 回退路径
)

// 工厂模块期望校验的错误哨兵（包级共用，bloom/cuckoo 两侧工厂均返回；
// 无组件前缀——错误文案不绑死单一组件，组件语境由包装层附加）。
var (
	// ErrCapabilityNotProbed 显式要求模块路径（BloomModuleBF/CuckooModuleCF）
	// 但 Capability 尚未完成一次成功探测——静默回退会在错误路径上对既有
	// 模块键发 STRLEN/GETBIT 报 WRONGTYPE（v0.10.x 事故），故 fail-loud
	// 指路先 Probe。
	ErrCapabilityNotProbed = errors.New("redis: capability not probed; call Capability().Probe(ctx) before constructing with a module requirement")
	// ErrModuleNotLoaded 已探测但要求的模块/命令族不在场（错误串携带
	// 实际判定结果）。
	ErrModuleNotLoaded = errors.New("redis: required module not loaded")
)

// BloomModule 声明 NewBloomFilter 期望的实现路径（零值 Auto=现状自动
// 分派；BF=必须 BF.* 模块路径，探测不满足即报错；Bitmap=无条件强制
// bitmap 布局，唯一正当用例是有 bf 的服务器上刻意做双路径对照/存量
// 迁移，路径错配事故由构造即连接的 WRONGTYPE fail-loud 兜底）。
type BloomModule uint8

const (
	BloomModuleAuto   BloomModule = iota // 零值：按 Capability 自动分派（v0.10.0 行为）
	BloomModuleBF                        // 强制 BF.* 路径：未探测/无 bf 模块一律构造期报错
	BloomModuleBitmap                    // 强制 bitmap 路径：不查探测状态、不报错
)

// WithModule 声明 Bloom 工厂的模块期望（校验发生在建连之前，失败零
// 命令副作用；分派规则见 BloomModule godoc）。不加本 Option 即 Auto，
// 行为与 v0.10.0 一致。
func WithModule(m BloomModule) BloomOption {
	return func(c *bloomConfig) { c.module = m }
}

// checkModuleRequirement 工厂「显式要求模块路径」的构造期前置校验
// （connectAll 之前调用，失败即返回错误、零命令副作用）：未探测 →
// 包装 ErrCapabilityNotProbed（指路 Probe）；已探测但 loaded=false →
// 包装 ErrModuleNotLoaded 并携带实际判定字样。bloom（HasBloom）与
// cuckoo（HasCuckoo）两侧共用。
func checkModuleRequirement(cap *Capability, loaded bool, want, verdict string) error {
	if !cap.Probed() {
		return fmt.Errorf("redis: %s requested: %w", want, ErrCapabilityNotProbed)
	}
	if !loaded {
		return fmt.Errorf("redis: %s requested, but probe verdict %s==false: %w", want, verdict, ErrModuleNotLoaded)
	}
	return nil
}

// --- Options ---

// BloomOption configures a Bloom filter.
type BloomOption func(*bloomConfig)

type bloomConfig struct {
	failPolicyConfig
	prefillConfig // 预填充门控配置（fn==nil 时未启用，工厂不包装）
	capacity      int64
	falsePositive float64
	shardCount    int         // 集群分片数（默认 1=关闭；仅 ModeCluster 且 n>1 生效；<=0 非法值被忽略）
	module        BloomModule // 模块期望（零值 Auto=自动分派；见 WithModule/BloomModule）
}

// BloomConfig 是 BloomFilter 的配置类型别名，供 WithFailPolicy 泛型参数使用。
type BloomConfig = bloomConfig

func defaultBloomConfig() bloomConfig {
	return bloomConfig{
		prefillConfig: defaultPrefillConfig(),
		capacity:      1000000,
		falsePositive: 0.0001, // 默认 0.01%
		shardCount:    defaultBloomShardCount,
	}
}

// WithCapacity sets the expected number of items.
// 非法值（n <= 0）静默忽略、保留默认 1000000。
// 所有形态首条写入前恒预建（BF.* 恒 BF.RESERVE、bitmap 恒 SETBIT 预热建
// 结构）；未显式传本 Option 时按默认 1000000/0.0001（0.01%）执行，
// 见 BloomFilter.Reserve。
// 位图容量创建时固定、不会扩容，上限 2^32-1 bit（Redis 字符串大小限制），
// p=0.01 时 capacity 超过约 4.5 亿、默认 p=0.0001 时超过约 2.24 亿将
// fail-fast panic；超容后误判率单调恶化且不可恢复，请预估充足容量或部署
// RedisBloom。
// 集群分片模式下 n 是全局总容量，均摊到各分片键（每分片容量下限 1000，
// 不足则分片数收缩，见 bloom_shard.go）。
func WithCapacity(n int64) BloomOption {
	return func(c *bloomConfig) {
		if n > 0 {
			c.capacity = n
		}
	}
}

// WithFalsePositive sets the desired false positive rate (0 < rate < 1).
// 非法值静默忽略、保留默认 0.0001（0.01%）。
// 所有形态首条写入前恒预建（BF.* 恒 BF.RESERVE、bitmap 恒 SETBIT 预热建
// 结构）；未显式传本 Option 时按默认值执行，见 BloomFilter.Reserve。
func WithFalsePositive(rate float64) BloomOption {
	return func(c *bloomConfig) {
		if rate > 0 && rate < 1 {
			c.falsePositive = rate
		}
	}
}

// WithShardCount 设置集群模式下的分片键数，默认 1 即不分片（v0.5.0 起分片
// 为显式 opt-in）；仅当 Mode()==ModeCluster 且 n>1 时启用：物理键 =
// <base>#<idx>，idx ∈ [0, effectiveN)；effectiveN 受每分片容量下限收缩，
// 实际分片数可能小于 n（见 bloom_shard.go）。非法值（n <= 0）静默忽略、
// 保留默认 1。选型建议：分片用于突破单键 512MB 容量上限与跨节点摊负载；
// 批量操作密集场景建议 n 取小（每多一组批量命令多一条命令，压测 8 分片
// 批量约 -45%，见 README 性能章节）。
// 若 base 键自带 {hashtag}，所有分片键路由到同一 slot——分片退化为
// 纯命名拆分，行为仍正确，但失去跨节点打散的意义。
func WithShardCount(n int) BloomOption {
	return func(c *bloomConfig) {
		if n > 0 {
			c.shardCount = n
		}
	}
}

// WithPrefill 声明式启用 Bloom 预填充门控：新建/重置后由库负责预填充
// 状态管理（Redis 权威状态键）、抢占重建、退避重试与多实例降级同步，
// 完成前 Exists 面按"未就绪恒存在"降级（见 bloom_prefill_filter.go）。
// fn==nil 静默忽略（不启用，对齐非法值静默忽略惯例）。opts 微调行为，
// 非法值同样静默忽略。
//
// PrefillFunc 契约（应用回灌函数）：
//   - 必须幂等可重试：库可能因退避/抢占在清空后再次调用；
//   - 以权威数据源为扫描基准：清空窗口内的增量写入由回灌补回；
//     假空防御：扫描成功但结果为空同样会置 ready——库无法区分"数据源
//     真空"与"读失败被吞成空集"，fn 必须以可达性哨兵（如先读元数据行/
//     心跳表，哨兵失败即返回 error）把"可疑的空"转为 error，交由库的
//     fail+退避接管；假空会放行"ready+空 bloom→Exists 真实查询全
//     false→洪泛回源"事故；
//   - 应响应 ctx 取消：预算为 RebuildTimeout，超时按失败计并指数退避；
//     ctx 纪律：fn 内部所有 I/O（DB 扫描、批量写入）必须挂在传入的
//     ctx 上——自建 context.Background()/独立超时会使 Close 与
//     RebuildTimeout 取消失效，超时/关闭后扫描继续空跑、破坏"何时算
//     成功回灌"语义；
//   - panic 由库 recover 转 error 走 fail 路径（不崩溃进程）。
func WithPrefill(fn PrefillFunc, opts ...PrefillOption) BloomOption {
	return func(c *bloomConfig) {
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

// WithRebuildTimeout 设置单次预填充重建的预算，默认 5 分钟；
// <=0 静默忽略。权威状态键的 building TTL = 该值 +
// max(10s, 2×syncInterval+5s) 裕量（见 prefillBuildingTTL）。
func WithRebuildTimeout(d time.Duration) PrefillOption {
	return func(c *prefillConfig) {
		if d > 0 {
			c.rebuildTimeout = d
		}
	}
}

// WithRetryBackoff 设置预填充失败的指数退避：backoff(n)=
// min(initial×2^(n-1), max)，multiplier 固定 2 不可配；默认 5s/10min；
// 非法值（<=0）静默忽略保留对应默认。
func WithRetryBackoff(initial, max time.Duration) PrefillOption {
	return func(c *prefillConfig) {
		if initial > 0 {
			c.retryInitial = initial
		}
		if max > 0 {
			c.retryMax = max
		}
	}
}

// WithSyncInterval 设置本地状态同步间隔，默认 1s；<=0 静默忽略。
// 该值同时影响三处：Δ 传播等待与 stale 阈值均为 2×该值，且 building TTL
// 的裕量随其放大（裕量=max(10s, 2×syncInterval+5s)，见 prefillBuildingTTL）。
func WithSyncInterval(d time.Duration) PrefillOption {
	return func(c *prefillConfig) {
		if d > 0 {
			c.syncInterval = d
		}
	}
}

// --- Factory ---

// NewBloomFilter creates a BloomFilter for the given key. The implementation
// is auto-selected based on the server's capabilities（HasBloom() 为真走
// BF.*，否则走 bitmap 回退；两路径数据布局不互通，强制路径与探测防线
// 见 WithModule/BloomModule）。
// 失效兜底策略默认 FailOpen（服务不可用时放行业务）；可用 WithFailPolicy
// 显式改为 FailClosed。
// 集群分片默认关闭；Mode()==ModeCluster 且显式 WithShardCount(n>1) 时打散
// 为多个 <base>#<idx> 物理键（见 WithShardCount、bloom_shard.go）。
//
// 构造即连接：对每个物理键同步执行建键/校验（BF.* 为 BF.RESERVE <fp>
// <每分片容量>；bitmap 为服务端原子脚本——空键 SETBIT 末位全额分配
// ⌈m/8⌉ 字节、同布局键复用、异布局报 mismatch），全部成功后才返回实例；
// 失败返回 (nil, err)，不交付半初始化实例。已存在键仅复用、不改写其
// 参数；BF.* 路径的 falsePositive 无法从服务端回读核验（BF.INFO 不回该
// 字段），复用既有键时 fp 一致性属界外。bitmap 布局不符或键类型不符报
// 数据类错误（含键名与期望长度，Reset 或换键解决）；服务不可用类错误包
// ErrRedisUnavailable 哨兵（errors.Is 可感知）。构造失败不随 FailPolicy
// 兜底——工厂返回的就是错误本身。
//
// 模块期望（v0.11.0 G4，见 WithModule/BloomModule）：默认 Auto 即上述
// 自动分派；BloomModuleBF 要求构造前已 Capability().Probe(ctx) 且探测
// 到 bf 模块，否则分别报 ErrCapabilityNotProbed / ErrModuleNotLoaded
// （校验在建连前，零命令副作用）；BloomModuleBitmap 无条件强制 bitmap
// 路径（双路径对照/存量迁移用，不查探测状态）。
func (rdb *redisClient) NewBloomFilter(ctx context.Context, key string, opts ...BloomOption) (BloomFilter, error) {
	cfg := defaultBloomConfig()
	cfg.policy = FailOpen // 过滤器默认 FailOpen：宁可放行不阻塞业务
	for _, o := range opts {
		o(&cfg)
	}

	// 模块期望校验（G4）：发生在建连之前——校验失败零命令副作用
	// （不发 RESERVE/不建键）。Auto 与 v0.10.0 逐行为一致；BF 要求
	// 已探测且 bf 模块在场；Bitmap 无条件强制（不查探测状态）。
	var inner BloomFilter
	switch cfg.module {
	case BloomModuleBF:
		if err := checkModuleRequirement(rdb.cap, rdb.cap.HasBloom(), "bloom BF.* path", "HasBloom()"); err != nil {
			return nil, err
		}
		bf := rdb.newBFImpl(key, cfg)
		if err := bf.connectAll(ctx); err != nil {
			return nil, err
		}
		inner = bf
	case BloomModuleBitmap:
		b := newBitmapImpl(rdb, key, cfg)
		if err := b.connectAll(ctx); err != nil {
			return nil, err
		}
		inner = b
	default: // BloomModuleAuto：按能力缓存自动分派（现状行为）
		if rdb.cap.HasBloom() {
			bf := rdb.newBFImpl(key, cfg)
			if err := bf.connectAll(ctx); err != nil {
				return nil, err
			}
			inner = bf
		} else {
			b := newBitmapImpl(rdb, key, cfg)
			if err := b.connectAll(ctx); err != nil {
				return nil, err
			}
			inner = b
		}
	}

	// 预填充门控：启用且 fn!=nil 才包装装饰器；未启用返回值与现网行为
	// 完全一致（直接交付 inner）。
	if cfg.enabled && cfg.fn != nil {
		return newPrefillFilter(rdb, inner, key, cfg), nil
	}
	return inner, nil
}

// NewBloomFilterWithEstimate creates a BloomFilter with explicit capacity and
// false positive probability. 等价于
// NewBloomFilter(ctx, key, WithCapacity(capacity), WithFalsePositive(falsePositive))，
// 构造即连接、失败返回错误（语义与限制同 NewBloomFilter，含 BF.* fp
// 不可服务端核验的边界）。
//
// 需要分片数、兜底策略等选项时，直接走 NewBloomFilter 组合对应 Option。
func (rdb *redisClient) NewBloomFilterWithEstimate(ctx context.Context, key string, capacity int64, falsePositive float64) (BloomFilter, error) {
	return rdb.NewBloomFilter(ctx, key,
		WithCapacity(capacity),
		WithFalsePositive(falsePositive),
	)
}

// bloomBitCount computes optimal bitmap size (m bits) using:
//
//	m = -n * ln(p) / (ln(2))^2
//
// 浮点值超出 uint64 可表示范围时饱和为 MaxUint64（float→uint64 溢出为实现
// 定义行为，可能截断为小值绕过 newBitmapImpl 的上限检查）；饱和值同样
// >2^32-1，由调用方 panic 兜底。
func bloomBitCount(n int64, p float64) uint64 {
	m := -float64(n) * math.Log(p) / (math.Ln2 * math.Ln2)
	if m > 1<<63 {
		return math.MaxUint64
	}
	return uint64(math.Ceil(m))
}

// bloomHashCount computes optimal number of hash functions using:
//
//	k = (m / n) * ln(2)
//
// 调用方须保证 n>0。
func bloomHashCount(n int64, m uint64) uint {
	if n <= 0 {
		return 1 // 防御：除零产生 +Inf 会被静默钳到 30；当前调用链保证 n≥1
	}
	k := float64(m) / float64(n) * math.Ln2
	if k < 1 {
		return 1
	}
	if k > 30 {
		return 30
	}
	return uint(math.Ceil(k))
}
