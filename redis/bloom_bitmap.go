package redis

import (
	"context"
	"fmt"
	"math"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeebo/xxh3"
)

type bitmapImpl struct {
	client        *redisClient
	sharder       bloomSharder
	m             uint64 // bitmap size in bits（本分片位图，按每分片容量计算）
	k             uint   // number of hash functions
	capacity      int64  // 每分片容量（m/k 布局依据，estimateNumItems 钳制上界；非分片时即总容量）
	totalCapacity int64
	policy        FailPolicy // 失效兜底策略（默认 FailOpen）
}

// newBitmapImpl 构造 bitmap 路径实现。集群模式显式开启分片
// （WithShardCount(n>1)）时：位图参数 m/k 按**每分片容量**
// ceil(总容量/effectiveN) 计算（路由/分组/键名共享层见 bloom_shard.go）；
// 关闭分片（默认、非集群或未显式请求）时不分片、每分片容量即总容量，
// 行为与键名同分片化之前完全一致。
func newBitmapImpl(client *redisClient, key string, cfg bloomConfig) *bitmapImpl {
	// client 为 nil 是内部纯计算模拟（仅调 hashs/m/k 等不触网方法，见
	// bloom_internal_test.go 的 newSimBitmap）：无模式可判，按非分片处理。
	mode := ModeStandalone
	if client != nil {
		mode = client.Mode()
	}
	enabled, n, perShard := resolveBloomSharding(mode, cfg.shardCount, cfg.capacity)

	m := bloomBitCount(perShard, cfg.falsePositive)
	// m 奇化：与双哈希步长强制奇（h2 |= 1，见 hashs）联合消除步长与模数的
	// 公因子 2，避免位轨道减半（belt-and-braces，详见 hashs 注释）。
	m |= 1

	// Redis 字符串（位图）大小上限 512MB = 2^32-1 bit，SETBIT 偏移超出即非法。
	// capacity 合法（>0）但过大时 fail-fast（panic 先例见 MustConstraint，
	// redis.go）：静默截断会让位图语义悄悄损坏，比崩溃更危险。
	// 注意：分片后该检查按**每分片容量**判定——同一总容量下 m 分摊到
	// effectiveN 个键，触发阈值随每分片容量缩小而位移（约为非分片时的
	// effectiveN 倍总容量），单个大容量过滤器经分片可绕开 512MB 单键上限。
	if m > math.MaxUint32 {
		panic("redis: bitmap bloom 超 Redis 位图上限 2^32-1，请降低 capacity 或部署 RedisBloom 模块")
	}

	// k 依赖 m/n，必须基于奇化后的 m 与每分片容量计算
	k := bloomHashCount(perShard, m)

	return &bitmapImpl{
		client:        client,
		sharder:       newBloomSharder(key, enabled, n),
		m:             m,
		k:             k,
		capacity:      perShard,
		totalCapacity: cfg.capacity,
		policy:        cfg.policy,
	}
}

// fallbackBool 按策略返回布隆过滤器兜底值 + 哨兵错误（与 bfCmdImpl 语义一致）。
func (b *bitmapImpl) fallbackBool(err error) (bool, error) {
	if b.policy == FailOpen {
		return true, fallbackErr(err)
	}
	return false, fallbackErr(err)
}

// fallbackBools 返回 AddMulti/ExistsMulti 的兜底切片（与 bfCmdImpl 语义一致）：
// FailOpen → 全 true；FailClosed → 全 false。集群分片下任一分片组服务
// 不可用即整体兜底——禁止"部分真实结果部分兜底"的混合输出。
func (b *bitmapImpl) fallbackBools(n int, err error) ([]bool, error) {
	res := make([]bool, n)
	for i := range res {
		res[i] = b.policy == FailOpen
	}
	return res, fallbackErr(err)
}

// hashs returns the k bit positions for an item.
// Uses double hashing (Kirsch-Mitzenmacher): h(i) = h1 + i * h2 (mod m)
func (b *bitmapImpl) hashs(item string) []uint64 {
	sum := xxh3.Hash128([]byte(item))
	h1 := sum.Hi
	h2 := sum.Lo | 1 // 步长强制奇：见上方注释

	positions := make([]uint64, b.k)
	for i := uint(0); i < b.k; i++ {
		positions[i] = (h1 + uint64(i)*h2) % b.m
	}
	return positions
}

var (
	// bitmapAddScript 原子添加：逐位 GETBIT 检查、SETBIT 置位。返回 1 表示
	// 调用前该 item 不可能存在（k 位中至少一位原为 0，缺位已全部置 1），
	// 对齐 BF.ADD 语义；返回 0 表示 k 位全为 1（可能存在，无缺位可补）。
	// "新增"判定基于置位之前的快照：先 GETBIT 后 SETBIT，本次调用自己置的
	// 位不参与判定，与兜底路径 addFallback 的判据（SETBIT 返回的旧值，
	// 任一原为 0 即新增）完全一致。
	bitmapAddScript = goredis.NewScript(`
		local added = 0
		for i = 1, #ARGV do
			if redis.call('GETBIT', KEYS[1], ARGV[i]) == 0 then
				redis.call('SETBIT', KEYS[1], ARGV[i], 1)
				added = 1
			end
		end
		return added
	`)

	// bitmapExistsScript 原子存在性检查：任一位置为 0 即不存在，返回 0。
	bitmapExistsScript = goredis.NewScript(`
		for i = 1, #ARGV do
			if redis.call('GETBIT', KEYS[1], ARGV[i]) == 0 then
				return 0
			end
		end
		return 1
	`)

	// bitmapAddMultiScript / bitmapExistsMultiScript 批量版（C3b）：
	// 单 KEYS[1]，ARGV = {k, item1 的 k 个位置…, item2 的 k 个位置…, ...}，
	// 返回 n 个 0/1。**输出按 ARGV 游标 idx 顺序生成，与 items 入参顺序
	// 严格对应**（对齐 BF.MADD 语义，见 bfCmdImpl.AddMulti）；while 游标
	// 结构不做整除运算，天然保持一项一输出。
	//
	// 不分块：k≤30 已在 bloomHashCount 封顶（bloom.go），单次脚本的循环上界
	// = n×k 个位操作；超大 n 时单次 Lua 在服务端的执行会阻塞该实例的事件
	// 循环（O(n·k) 往返型命令成本），由调用方自行控制批量大小。
	//
	// 新增判据与 bitmapAddScript 同构：某 item 的 k 位中至少一位原为 0
	// （调用前不可能存在）即输出 1，否则输出 0。
	bitmapAddMultiScript = goredis.NewScript(`
		local k = tonumber(ARGV[1])
		local out = {}
		local idx = 2
		while idx <= #ARGV do
			local added = 0
			for j = 0, k - 1 do
				if redis.call('GETBIT', KEYS[1], ARGV[idx + j]) == 0 then
					redis.call('SETBIT', KEYS[1], ARGV[idx + j], 1)
					added = 1
				end
			end
			out[#out + 1] = added
			idx = idx + k
		end
		return out
	`)

	bitmapExistsMultiScript = goredis.NewScript(`
		local k = tonumber(ARGV[1])
		local out = {}
		local idx = 2
		while idx <= #ARGV do
			local found = 1
			for j = 0, k - 1 do
				if redis.call('GETBIT', KEYS[1], ARGV[idx + j]) == 0 then
					found = 0
					break
				end
			end
			out[#out + 1] = found
			idx = idx + k
		end
		return out
	`)
)

// positions 将 item 的 k 个哈希位转为脚本参数（[]uint64 → []interface{}）。
func (b *bitmapImpl) positions(item string) []interface{} {
	hashs := b.hashs(item)
	args := make([]any, len(hashs))
	for i, p := range hashs {
		args[i] = p
	}
	return args
}

// multiPositionsArgs 组装批量脚本参数：ARGV = {k, item1 位…, item2 位…, ...}，
// 位置按 items 入参顺序展开——脚本按序输出，返回与入参严格对应。
func (b *bitmapImpl) multiPositionsArgs(items []string) []any {
	args := make([]any, 0, 1+len(items)*int(b.k))
	args = append(args, b.k)
	for _, item := range items {
		for _, p := range b.hashs(item) {
			args = append(args, p)
		}
	}
	return args
}

// scriptOutcome 是一次位图 Lua 脚本尝试（含入口记忆分派）的处置结论。
type scriptOutcome uint8

const (
	// scriptOK 脚本执行成功，结果在返回的 cmd 中（并已学习记忆"支持"）。
	scriptOK scriptOutcome = iota
	// scriptFallback 服务器不支持 Lua（记忆已/将置 -1，或入口已知 -1）：
	// 调用方降级到非原子回退路径（pipeline 兜底）。
	scriptFallback
	// scriptUnavailable 瞬态错误（记忆不动）：调用方按 FailPolicy 兜底。
	scriptUnavailable
	// scriptError 数据类错误（记忆不动）：原样返回给调用方。
	scriptError
)

// runBitmapScript 按 redisClient.luaSupport 三态记忆分派执行位图 Lua 脚本
// （单 KEYS[1]，所有命令同 key——分片态下 key 传入单个分片物理键，同键
// 同 slot，cluster 合法；客户端不算 slot，go-redis 自动路由）。
func (b *bitmapImpl) runBitmapScript(ctx context.Context, key string, s *goredis.Script, args []interface{}) (*goredis.Cmd, scriptOutcome) {
	if !b.client.luaTryEval() {
		return nil, scriptFallback
	}
	cmd := s.Run(ctx, b.client, []string{key}, args...)
	err := cmd.Err()
	if err == nil {
		b.client.luaMarkSupported()
		return cmd, scriptOK
	}
	switch classifyLuaError(err) {
	case luaVerdictUnsupported:
		b.client.luaMarkUnsupported()
		return cmd, scriptFallback
	case luaVerdictUnavailable:
		return cmd, scriptUnavailable
	default: // luaVerdictDataError
		return cmd, scriptError
	}
}

func (b *bitmapImpl) add(ctx context.Context, item string) (bool, error) {
	cmd, outcome := b.runBitmapScript(ctx, b.sharder.keyFor(item), bitmapAddScript, b.positions(item))
	switch outcome {
	case scriptOK:
		added, err := cmd.Int()
		if err != nil {
			return false, err
		}
		// Lua 返回 1 = 调用前该 item 不可能存在（k 位未全部置 1），对齐 BF.ADD 语义
		return added == 1, nil
	case scriptUnavailable:
		// 服务不可用：直接兜底（fallback 同样会失败）
		return b.fallbackBool(cmd.Err())
	case scriptError:
		// 数据类错误（WRONGTYPE 等）：原样返回，不降级（降级也只会重复报错，
		// 且不得干扰 Lua 能力记忆）
		return false, cmd.Err()
	default: // scriptFallback：服务器不支持 Lua，回退到非原子多命令实现
		return b.addFallback(ctx, item)
	}
}

// addFallback 非原子回退：k 个 SETBIT 合并为 1 次 pipeline，利用 SETBIT
// 返回的置位前旧值直接判定"是否新增"，无需预检查存在性。
// 全部命令作用于该 item 所在的分片物理键（未分片时即 base），同键同
// slot，cluster 合法。
//
// 本实现由旧版双往返（先 existsFallback pipeline 查询、再 SetBit pipeline
// 写入）优化为单往返（仅 SetBit pipeline，以 SETBIT 返回的置位前旧值判定
// 新增：任一旧值为 0 → 新增），消除检查与写入之间的 TOCTOU 窗口；判定语义
// 由"调用前不存在"变为"至少一位原为 0"，在 fallback（非 Lua）路径下等价
// 且更接近原子。
func (b *bitmapImpl) addFallback(ctx context.Context, item string) (bool, error) {
	hashs := b.hashs(item)
	key := b.sharder.keyFor(item)

	pipe := b.client.Pipeline()
	cmds := make([]*goredis.IntCmd, len(hashs))
	for i, pos := range hashs {
		cmds[i] = pipe.SetBit(ctx, key, int64(pos), 1)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		if IsUnavailable(err) {
			return b.fallbackBool(err)
		}
		return false, err
	}

	// 任一旧位值为 0 → 调用前 k 位未全部置 1，该 item 不可能存在，
	// 返回 true——与 Lua 脚本及 BF.ADD 判据一致；非"k 位全为 0"。
	for _, c := range cmds {
		if c.Val() == 0 {
			return true, nil
		}
	}
	return false, nil
}

func (b *bitmapImpl) Add(ctx context.Context, item string) (bool, error) {
	return b.add(ctx, item)
}

func (b *bitmapImpl) exists(ctx context.Context, item string) (bool, error) {
	cmd, outcome := b.runBitmapScript(ctx, b.sharder.keyFor(item), bitmapExistsScript, b.positions(item))
	switch outcome {
	case scriptOK:
		exists, err := cmd.Int()
		if err != nil {
			return false, err
		}
		return exists == 1, nil
	case scriptUnavailable:
		// 服务不可用：直接兜底
		return b.fallbackBool(cmd.Err())
	case scriptError:
		// 数据类错误：原样返回，不降级
		return false, cmd.Err()
	default: // scriptFallback：服务器不支持 Lua，回退到多命令实现
		return b.existsFallback(ctx, item)
	}
}

// existsFallback 非原子回退：k 个 GETBIT 用 pipeline 合并为 1 次往返
// （命令同落该 item 的分片物理键，同 slot 合法）。任一位置为 0 即不存在。
func (b *bitmapImpl) existsFallback(ctx context.Context, item string) (bool, error) {
	hashs := b.hashs(item)

	pipe := b.client.Pipeline()
	cmds := make([]*goredis.IntCmd, len(hashs))
	key := b.sharder.keyFor(item)
	for i, pos := range hashs {
		cmds[i] = pipe.GetBit(ctx, key, int64(pos))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		if IsUnavailable(err) {
			return b.fallbackBool(err)
		}
		return false, err
	}

	for _, c := range cmds {
		if c.Val() == 0 {
			return false, nil
		}
	}
	return true, nil
}

func (b *bitmapImpl) Exists(ctx context.Context, item string) (bool, error) {
	return b.exists(ctx, item)
}

// AddMulti 批量添加：单物理键形态（standalone / 集群退化态）优先单次批量
// Lua 脚本（1 个 KEYS[1]，ARGV 含 k 与 n×k 个位置，返回 n 个 0/1，顺序与
// items 严格对应，对齐 BF.MADD 语义），批量脚本不可用（Lua 被禁/瞬态错误）
// 时逐条降级到现有单条路径（add 内部含三态分派与 fallbackBool 兜底）。
// 集群多分片形态：按分片分组、单个 Pipeline 提交 ≤effectiveN 条批量 EVAL，
// 结果按原始下标回填（见 multiSharded）。
// 不实现分块：k≤30 已封顶，超大 n 的单次 Lua 阻塞代价见脚本注释声明。
func (b *bitmapImpl) AddMulti(ctx context.Context, items ...string) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if b.sharder.enabled && b.sharder.n > 1 {
		if !b.client.luaTryEval() {
			// Lua 禁用降级：逐条且不可再套外层 pipeline——单条 add 内部
			// 自行路由分片与兜底，与现状 addMultiLoop 一致串行。
			return b.addMultiLoop(ctx, items)
		}
		return b.multiSharded(ctx, bitmapAddMultiScript, items, true)
	}
	return b.multiViaScript(ctx, b.sharder.shardKey(0), bitmapAddMultiScript, items, true)
}

// ExistsMulti 批量存在性检查：批量 Lua 脚本 / 分片 Pipeline，返回顺序与
// items 严格对应；失败时逐条降级到 exists（含三态分派与兜底）。空入参早返回。
func (b *bitmapImpl) ExistsMulti(ctx context.Context, items ...string) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if b.sharder.enabled && b.sharder.n > 1 {
		if !b.client.luaTryEval() {
			return b.existsMultiLoop(ctx, items)
		}
		return b.multiSharded(ctx, bitmapExistsMultiScript, items, false)
	}
	return b.multiViaScript(ctx, b.sharder.shardKey(0), bitmapExistsMultiScript, items, false)
}

// multiViaScript 单物理键批量脚本路径（runBitmapScript 三态分派 + 逐条
// 降级），standalone 行为与分片化之前逐语句一致（key 为 base；集群退化态
// 为 base#0，除键名外语义不变）。
func (b *bitmapImpl) multiViaScript(ctx context.Context, key string, script *goredis.Script, items []string, isAdd bool) ([]bool, error) {
	op := "ExistsMulti"
	if isAdd {
		op = "AddMulti"
	}

	cmd, outcome := b.runBitmapScript(ctx, key, script, b.multiPositionsArgs(items))
	switch outcome {
	case scriptOK:
		vals, err := cmd.Int64Slice()
		if err != nil {
			return nil, err
		}
		if len(vals) != len(items) {
			return nil, fmt.Errorf("redis: bitmap %s 脚本返回 %d 个结果，期望 %d", op, len(vals), len(items))
		}
		result := make([]bool, len(items))
		for i, v := range vals {
			result[i] = v == 1
		}
		return result, nil
	case scriptError:
		return nil, cmd.Err()
	default:
		// scriptFallback / scriptUnavailable：逐条降级（单条路径自行兜底）
		if isAdd {
			return b.addMultiLoop(ctx, items)
		}
		return b.existsMultiLoop(ctx, items)
	}
}

// multiSharded 多分片批量脚本：按分片分组（组内保留原始下标）后经**单个
// Pipeline** 提交 ≤effectiveN 条批量 EVAL（每组一条既有批量脚本），结果按
// 原始下标回填 []bool——返回顺序与入参 items 一一对应。无 goroutine、
// 无逐条循环。
//
// 为什么组命令用 inline EVAL 而非 EVALSHA：pipeline 内无法表达 NOSCRIPT
// 重试——EVALSHA 错误要 Exec 后才可见，同一 pipeline 不能补发 EVAL
// （go-redis Script.Run 的 NOSCRIPT 回退只覆盖非 pipeline 单命令）。
// 脚本体仅数百字节，内联的额外带宽成本可接受，是"单 Pipeline"约束下的
// 最优实现；单键路径（multiViaScript）仍走 EVALSHA+三态记忆。
//
// 失败语义（与 BF.* 路径一致，规格硬约束）：
//   - 任一分片组 IsUnavailable → 全部 items 按 policy 兜底（fallbackBools）
//   - 哨兵错误——禁止"部分真实部分兜底"的混合结果；
//   - 全部分片组均报"Lua 被禁/不支持"（服务器级属性，没有任何组真实执行，
//     重跑零副作用）→ 记忆置 -1 并逐条安全重试（与单键降级形态一致）；
//   - 其余数据类错误原样返回——其他组可能已写入：布隆置位幂等，整体重试
//     无数据危害（仅重试时"新增"返回值语义失准，见 AddMulti 接口注释）。
func (b *bitmapImpl) multiSharded(ctx context.Context, script *goredis.Script, items []string, isAdd bool) ([]bool, error) {
	op := "ExistsMulti"
	if isAdd {
		op = "AddMulti"
	}

	groups := b.sharder.group(items)
	pipe := b.client.Pipeline()
	cmds := make([]*goredis.Cmd, len(groups))
	for i, g := range groups {
		cmds[i] = script.Eval(ctx, pipe, []string{g.key}, b.multiPositionsArgs(g.items)...)
	}
	if _, err := pipe.Exec(ctx); err != nil && IsUnavailable(err) {
		return b.fallbackBools(len(items), err)
	}

	var firstErr error
	allUnsupported := true // 空真初值：全部 cmd 均判 Unsupported 时触发安全降级
	for _, c := range cmds {
		err := c.Err()
		if err == nil {
			allUnsupported = false
			continue
		}
		if IsUnavailable(err) {
			return b.fallbackBools(len(items), err)
		}
		if classifyLuaError(err) != luaVerdictUnsupported {
			allUnsupported = false
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if allUnsupported && firstErr != nil {
		b.client.luaMarkUnsupported()
		if isAdd {
			return b.addMultiLoop(ctx, items)
		}
		return b.existsMultiLoop(ctx, items)
	}
	if firstErr != nil {
		return nil, firstErr
	}

	result := make([]bool, len(items))
	for i, g := range groups {
		vals, err := cmds[i].Int64Slice()
		if err != nil {
			return nil, err
		}
		if len(vals) != len(g.items) {
			return nil, fmt.Errorf("redis: bitmap %s 分片 %d 脚本返回 %d 个结果，期望 %d", op, g.idx, len(vals), len(g.items))
		}
		for j, v := range vals {
			result[g.srcIdx[j]] = v == 1 // 按原始下标回填，顺序与入参对应
		}
	}
	return result, nil
}

// addMultiLoop 逐条走单条 add；任一条错误即返回（与历史逐条实现语义一致）。
func (b *bitmapImpl) addMultiLoop(ctx context.Context, items []string) ([]bool, error) {
	result := make([]bool, len(items))
	for i, item := range items {
		added, err := b.add(ctx, item)
		if err != nil {
			return nil, err
		}
		result[i] = added
	}
	return result, nil
}

// existsMultiLoop 逐条走单条 exists；任一条错误即返回（与历史实现一致）。
func (b *bitmapImpl) existsMultiLoop(ctx context.Context, items []string) ([]bool, error) {
	result := make([]bool, len(items))
	for i, item := range items {
		exists, err := b.exists(ctx, item)
		if err != nil {
			return nil, err
		}
		result[i] = exists
	}
	return result, nil
}

// Info 返回 bitmap 路径的元数据估算：NumItems 由 BITCOUNT 置位数反推。
// 注意 BITCOUNT 为 O(bytes) 全量扫描（每分片位图上限 512MB），属重命令，
// 仅适合低频运维查询，勿在热路径调用。
func (b *bitmapImpl) Info(ctx context.Context) (*BloomInfo, error) {
	agg := &BloomInfo{Capacity: b.totalCapacity}
	for _, key := range b.sharder.allKeys() {
		strLen, err := b.client.StrLen(ctx, key).Result()
		if err != nil {
			if IsUnavailable(err) {
				// Info 非关键：兜底返回空结构体 + 哨兵错误（errors.Is 可感知）
				return &BloomInfo{}, fallbackErr(err)
			}
			return nil, err
		}

		bitsSet, err := b.client.BitCount(ctx, key, nil).Result()
		if err != nil {
			if IsUnavailable(err) {
				return &BloomInfo{}, fallbackErr(err)
			}
			return nil, err
		}

		agg.Size += strLen
		agg.NumItems += b.estimateNumItems(bitsSet)
	}
	return agg, nil
}

// estimateNumItems 由置位数反推已插入元素数（标准 Bloom filter 估计量）：
//
//	numItems ≈ -(m / k) * ln(1 - bitsSet / m)
//
// bitsSet >= m（位图饱和）时 ln 参数 <= 0 会产生 NaN/Inf，钳制到配置容量
// 上界——此时真实插入数已超容、估计量失效（见 WithCapacity 容量契约）。
func (b *bitmapImpl) estimateNumItems(bitsSet int64) int64 {
	fraction := 1 - float64(bitsSet)/float64(b.m)
	if fraction <= 0 {
		return b.capacity
	}
	return int64(math.Round(-(float64(b.m) / float64(b.k)) * math.Log(fraction)))
}
