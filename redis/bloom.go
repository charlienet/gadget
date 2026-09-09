package redis

import (
	"context"
	"math"
)

// BloomFilter 公共面：接口定义、配置与 Option、工厂入口，
// 以及两实现共享的容量数学（bloomBitCount/bloomHashCount）。
// BF.* 命令实现见 bloom_bf.go；bitmap/Lua 实现见 bloom_bitmap.go；
// 集群分片路由共享层见 bloom_shard.go。

// BloomFilter 是 Redis 支撑的布隆过滤器：服务器加载 RedisBloom 模块时
// 使用原生 BF.* 命令，否则回退到 bitmap（GETBIT/SETBIT + Lua）实现，
// 应用层无需检查 HasBloom()。
//
// 容量契约（bitmap 路径）：容量在创建时固定，位图不会扩容。插入量超过
// 预估容量后误判率单调恶化且不可恢复（布隆无删除语义），属应用端容量
// 规划责任；对策为预留充足容量、周期性重建（新实例换新键，或 Reset 就地
// 清空复用同一实例），或部署 RedisBloom 模块（BF.* 路径支持自动扩容）。
// Reset 在集群分片下非原子（见 Reset 方法注释）。本库不封装 BF.INSERT：
// 预分配一律经 WithCapacity/WithEstimate 惰性 RESERVE；bitmap 回退路径
// 首写天然 autocreate。
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

	// Reset 清空过滤器的全部物理键，计数归零，实例约束（容量/FPR）不变；
	// bfCmdImpl 分片模式下后续首次写入会重新惰性执行 BF.RESERVE。
	//
	// 语义与限制：
	//   - 单键形态（未分片）：DEL <base>，Redis 单命令原子。
	//   - 集群分片：逐分片 DEL <base>#<idx>——跨 slot 无法原子，返回错误时
	//     可能只清空部分分片；DEL 幂等，可安全重试直至成功。
	//   - 幂等：分片键不存在时返回 nil（对齐 Redis DEL 语义）。
	//   - 并发：与 Add/Exists 无全序保证，读者可能观察到旧存在性或中间态；
	//     需要强一致清空的场景请改用全新键 + 指针原子替换。
	//   - 失败恒返回错误（errors.Is(ErrRedisUnavailable) 可感知），
	//     与 FailPolicy 取值无关。
	//   - Reset 删除整个键——勿与布隆过滤器共用键。
	Reset(ctx context.Context) error
}

// BloomInfo contains metadata about a Bloom filter.
// bitmap 回退版仅 Capacity/Size/NumItems 有效，NumFilters/Expansion
// 恒 0（不适用）。
type BloomInfo struct {
	Capacity   int64 // configured capacity
	Size       int64 // memory size (bytes)
	NumFilters int64 // number of filters (仅 BF.* 路径有效，bitmap 回退版恒 0)
	NumItems   int64 // approximate number of items
	Expansion  int64 // expansion factor (仅 BF.* 路径有效，bitmap 回退版恒 0)
}

// --- Options ---

// BloomOption configures a Bloom filter.
type BloomOption func(*bloomConfig)

type bloomConfig struct {
	failPolicyConfig
	capacity      int64
	falsePositive float64
	shardCount    int // 集群分片数（默认 1=关闭；仅 ModeCluster 且 n>1 生效；<=0 非法值被忽略）
}

// BloomConfig 是 BloomFilter 的配置类型别名，供 WithFailPolicy 泛型参数使用。
type BloomConfig = bloomConfig

func defaultBloomConfig() bloomConfig {
	return bloomConfig{
		capacity:      1000000,
		falsePositive: 0.01,
		shardCount:    defaultBloomShardCount,
	}
}

// WithCapacity sets the expected number of items.
// 非法值（n <= 0）静默忽略、保留默认 1000000。
// 位图容量创建时固定、不会扩容，上限 2^32-1 bit（Redis 字符串大小限制），
// p=0.01 时 capacity 超过约 4.5 亿将 fail-fast panic；超容后误判率单调
// 恶化且不可恢复，请预估充足容量或部署 RedisBloom。
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

// --- Factory ---

// NewBloomFilter creates a BloomFilter for the given key. The implementation
// is auto-selected based on the server's capabilities（HasBloom() 为真走
// BF.*，否则走 bitmap 回退；不提供强制路径旋钮，两路径数据布局不互通，
// 双路径对照用两个不同 key 分别灌入）。
// 失效兜底策略默认 FailOpen（服务不可用时放行业务）；可用 WithFailPolicy
// 显式改为 FailClosed。
// 集群分片默认关闭；Mode()==ModeCluster 且显式 WithShardCount(n>1) 时打散
// 为多个 <base>#<idx> 物理键（见 WithShardCount、bloom_shard.go）；其余
// 情况键名与行为完全不变。
func (rdb *redisClient) NewBloomFilter(key string, opts ...BloomOption) BloomFilter {
	cfg := defaultBloomConfig()
	cfg.policy = FailOpen // 过滤器默认 FailOpen：宁可放行不阻塞业务
	for _, o := range opts {
		o(&cfg)
	}

	if rdb.cap.HasBloom() {
		return rdb.newBFImpl(key, cfg)
	}
	return newBitmapImpl(rdb, key, cfg)
}

// NewBloomFilterWithEstimate creates a BloomFilter with explicit capacity and
// false positive probability. 等价于
// NewBloomFilter(key, WithCapacity(capacity), WithFalsePositive(falsePositive))。
//
// BF.* 路径的预分配语义：仅集群分片模式下、每个分片键首次写入前惰性
// 执行一次 BF.RESERVE <base>#<idx> <falsePositive> <每分片容量>（见
// bfCmdImpl.reserveShard），使两参数真正生效；其余场景首条 BF.ADD 按
// RedisBloom 服务端默认容量自动创建，capacity/falsePositive 仅影响
// bitmap 路径的 m/k 布局。
//
// 需要分片数、兜底策略等选项时，直接走 NewBloomFilter 组合对应 Option。
func (rdb *redisClient) NewBloomFilterWithEstimate(key string, capacity int64, falsePositive float64) BloomFilter {
	return rdb.NewBloomFilter(key,
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
