package redis

import (
	"strconv"

	"github.com/zeebo/xxh3"
)

// 本文件是布隆过滤器的 Redis Cluster 分片共享层：路由、键名、分组与容量
// 分摊计算。bfCmdImpl（BF.* 原生命令）与 bitmapImpl（Lua/bitmap）两条实现
// 共享这里的全部逻辑——两条路径的分片行为（路由、键名、分组、结果回填、
// 错误语义）"完全一致"由同一份代码保证，而非两份复制实现。
//
// 设计要点（对应规格 1-3、5、6）：
//   - 仅 Mode()==ModeCluster 时启用分片；standalone/sentinel/ring 键名与
//     行为与分片化之前完全一致（enabled=false，shardKey 恒返回 base）。
//   - 物理键 = <base>#<idx>，idx ∈ [0, effectiveN)。集群下即使
//     effectiveN==1（退化态）也统一带 #0 后缀，保持键名连续。
//   - 路由 idx = xxh3.Hash128(item).Hi % effectiveN（与 bitmap 位哈希
//     同源 xxh3-128；不引入 CRC16 新实现、不给分片键加 hash tag——
//     客户端不算 slot，go-redis cluster 按整键自动路由）。
//   - cfg 总容量是全局量：每分片容量 = ceil(总/effectiveN)，且下限
//     minShardCapacity，算不出下限即收缩 effectiveN（见
//     bloomEffectiveShardCount）。

const (
	// defaultBloomShardCount 是集群模式下请求的分片键数（WithShardCount 可调）。
	defaultBloomShardCount = 8

	// minShardCapacity 是每个分片容量的硬下限。低于该值分片数收缩：
	// 过小分片会让 bitmap 路径 k=ln2·m/n 暴涨、负载不均导致的误判率恶化
	// 显著，并堵死 capacity<N 时整除为 0 的静默失真路径。
	minShardCapacity = 1000
)

// bloomShardKeySep 是分片物理键的索引分隔符：<base>#<idx>。
const bloomShardKeySep = "#"

// shardIndex 是纯路由函数：idx = xxh3.Hash128(item).Hi % n；n <= 1 恒 0
// （退化单分片）。无状态、可表驱动单测（确定性/均匀性/模映射）。
func shardIndex(item string, n int) int {
	if n <= 1 {
		return 0
	}
	sum := xxh3.Hash128([]byte(item))
	return int(sum.Hi % uint64(n))
}

// bloomEffectiveShardCount 计算有效分片数（容量收缩规则，硬约束）：
//
//	effectiveN = min(requested, totalCapacity/minShardCapacity)（下限 1）
//
// 由 effectiveN ≤ total/1000（下取整）可推出每分片容量
// ceil(total/effectiveN) ≥ 1000 恒成立。极端情况（总容量 < 2×1000）
// 无论请求多少分片都收缩为 effectiveN=1。
//
// 该收缩堵死"总容量 < 分片数时整除为 0"的静默失真路径：若不收缩，
// perShard=total/N 整除为 0 → m=bloomBitCount(0)=0→奇化 1 →
// k=bloomHashCount 除零防护取 30 → item 的 30 个探测位坍缩到同一位，
// 过滤器退化成单 bit 守门且无任何报错。
func bloomEffectiveShardCount(requested int, totalCapacity int64) int {
	if requested <= 1 {
		return 1
	}
	byCap := int(totalCapacity / minShardCapacity)
	if byCap < 1 {
		return 1
	}
	if byCap < requested {
		return byCap
	}
	return requested
}

// bloomPerShardCapacity 计算每分片容量 ceil(total/n)；n<=1 时即 total。
// ceil 用商+余数式实现——(total+n-1)/n 的经典写法在 total 逼近
// math.MaxInt64 时加法回绕溢出（会得到错误的极小值，绕过 newBitmapImpl
// 的 2^32 fail-fast 检查，正是 WithCapacity 注释警告的"静默截断"类失真），
// 商余式在 int64 全值域内无溢出。
func bloomPerShardCapacity(total int64, n int) int64 {
	if n <= 1 {
		return total
	}
	q := total / int64(n)
	if total%int64(n) != 0 {
		q++
	}
	return q
}

// resolveBloomSharding 按运行模式与配置计算分片全部参数（纯函数，便于
// 表驱动单测）。非集群模式分片关闭；集群模式即使 effectiveN==1 也返回
// enabled=true——物理键统一带 #0 后缀，保证集群键名格式连续（迁移/排查
// 时不必区分"集群单分片"与"未分片"两种命名）。
func resolveBloomSharding(mode Mode, requestedShards int, totalCapacity int64) (enabled bool, n int, perShard int64) {
	if mode != ModeCluster {
		return false, 1, totalCapacity
	}
	n = bloomEffectiveShardCount(requestedShards, totalCapacity)
	return true, n, bloomPerShardCapacity(totalCapacity, n)
}

// bloomSharder 是每个过滤器实例持有的分片路由/分组状态，构造后只读
// （值接收者方法，可安全随 impl 结构体复制）。
type bloomSharder struct {
	base    string
	enabled bool
	n       int // 有效分片数（≥1；enabled=false 时恒 1）
}

func newBloomSharder(base string, enabled bool, n int) bloomSharder {
	if !enabled {
		n = 1 // 关闭态单键：n 无分片含义，归一为 1（allKeys/group 语义即"唯一 base 键"）
	}
	if n < 1 {
		n = 1
	}
	return bloomSharder{base: base, enabled: enabled, n: n}
}

// shardKey 返回第 idx 个分片的物理键（未加前缀——前缀由 renameHook 在
// 命令层统一添加，分片后缀必须先于前缀拼在业务 key 上）。
func (s bloomSharder) shardKey(idx int) string {
	if !s.enabled {
		return s.base
	}
	return s.base + bloomShardKeySep + strconv.Itoa(idx)
}

// indexOf 计算 item 的分片下标；关闭态恒 0（与单键行为一致）。
func (s bloomSharder) indexOf(item string) int {
	if !s.enabled {
		return 0
	}
	return shardIndex(item, s.n)
}

// keyFor 组合路由与键名：item 所在的物理键。单条 Add/Exists 与分组
// 批量都经此口径，保证同一条目在任何路径落同一分片。
func (s bloomSharder) keyFor(item string) string {
	return s.shardKey(s.indexOf(item))
}

// allKeys 列出全部分片物理键（Info 聚合遍历用）。
func (s bloomSharder) allKeys() []string {
	keys := make([]string, s.n)
	for i := range keys {
		keys[i] = s.shardKey(i)
	}
	return keys
}

// shardGroup 是落入同一分片的 item 分组。srcIdx 记录组内每个 item 在
// 原始入参中的下标——批量结果必须按原始下标回填，返回顺序与入参 items
// 一一对应（对齐 BF.MADD 语义）。
type shardGroup struct {
	idx    int
	key    string
	items  []string
	srcIdx []int
}

// group 按分片下标对 items 分组，返回按 idx 升序的非空分组。
// 关闭态/单分片只返回一组且 srcIdx 为恒等序列（行为等价原单键批量）；
// 键名走 shardKey(0)——集群退化态同样得 <base>#0，与单条路径口径一致。
func (s bloomSharder) group(items []string) []shardGroup {
	if !s.enabled || s.n <= 1 {
		g := shardGroup{key: s.shardKey(0), items: items, srcIdx: make([]int, len(items))}
		for i := range items {
			g.srcIdx[i] = i
		}
		return []shardGroup{g}
	}

	buckets := make([]shardGroup, s.n)
	for i, item := range items {
		idx := shardIndex(item, s.n)
		g := &buckets[idx]
		if g.items == nil {
			g.idx = idx
			g.key = s.shardKey(idx)
		}
		g.items = append(g.items, item)
		g.srcIdx = append(g.srcIdx, i)
	}

	groups := make([]shardGroup, 0, s.n)
	for i := range buckets {
		if buckets[i].items != nil {
			groups = append(groups, buckets[i])
		}
	}
	return groups
}
