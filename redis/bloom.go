package redis

import (
	"context"
	"fmt"
	"math"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeebo/xxh3"
)

// BloomFilter 是 Redis 支撑的布隆过滤器：服务器加载 RedisBloom 模块时
// 自动使用原生 BF.* 命令，否则回退到 bitmap（GETBIT/SETBIT + Lua）实现。
// 应用层无需检查 HasBloom()——包装层已处理。
//
// 容量契约（bitmap 路径）：容量在创建时固定，位图不会扩容。插入量超过
// 预估容量后误判率按 (1-e^(-k·n'/m))^k 单调恶化且**不可恢复**（位图无删除
// 语义），属应用端容量规划责任；对策为预估充足容量、周期性重建过滤器，
// 或部署 RedisBloom 模块（BF.* 路径支持自动扩容子过滤器）。
//
// 哈希兼容性：v0.5.0 起 bitmap 路径的位图哈希由 FNV-1a 双哈希换为
// xxh3-128 双哈希，旧位图 key 对新代码会产生假阴性，升级时必须删除旧
// key 或更换 key 重建。BF.*（RedisBloom 模块）路径不受影响。
type BloomFilter interface {
	// Add adds an item to the filter. 返回 true 表示调用前该 item 不可能
	// 存在（其 k 个位未全部置 1），对齐 RedisBloom BF.ADD 语义；返回 false
	// 表示它可能已存在（k 位全为 1）。Returns true if the item could not
	// have existed before this call (at least one of its k bits was 0).
	Add(ctx context.Context, item string) (bool, error)

	// Exists checks whether an item has possibly been added to the filter.
	// Returns false if definitely not present; true if it may be present.
	Exists(ctx context.Context, item string) (bool, error)

	// AddMulti adds multiple items at once. Returns a slice of booleans
	// indicating whether each item was newly added.
	AddMulti(ctx context.Context, items ...string) ([]bool, error)

	// ExistsMulti checks multiple items at once.
	ExistsMulti(ctx context.Context, items ...string) ([]bool, error)

	// Info returns metadata about the Bloom filter.
	Info(ctx context.Context) (*BloomInfo, error)
}

// BloomInfo contains metadata about a Bloom filter.
type BloomInfo struct {
	Capacity   int64 // configured capacity
	Size       int64 // memory size (bytes)
	NumFilters int64 // number of filters (BF.* only)
	NumItems   int64 // approximate number of items
	Expansion  int64 // expansion factor (BF.* only)
}

// --- Options ---

// BloomOption configures a Bloom filter.
type BloomOption func(*bloomConfig)

type bloomConfig struct {
	failPolicyConfig
	capacity      int64
	falsePositive float64
}

// BloomConfig 是 BloomFilter 的配置类型别名，供 WithFailPolicy 泛型参数使用。
type BloomConfig = bloomConfig

func defaultBloomConfig() bloomConfig {
	return bloomConfig{
		capacity:      1000000,
		falsePositive: 0.01,
	}
}

// WithCapacity sets the expected number of items.
// 非法值（n <= 0）静默忽略、保留默认 1000000（与 WithFalsePositive 及
// cuckoo.go 的 WithCuckooCapacity 惯例一致）。
// 容量契约：位图容量创建时固定、不会扩容；bitmap 路径的位图上限为
// 2^32-1 bit（Redis 字符串大小限制），p=0.01 时 capacity 超过约 4.5 亿
// （m≈9.6n）将触发 newBitmapImpl 的 fail-fast panic。超容后误判率按
// (1-e^(-kn'/m))^k 单调恶化且不可恢复，请预估充足容量或部署 RedisBloom。
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

// --- Factory ---

// NewBloomFilter creates a BloomFilter for the given key. The implementation
// is auto-selected based on the server's capabilities.
// 失效兜底策略默认 FailOpen（过滤器是保护性能力：服务不可用时防穿透失效但
// 放行业务）；可用 WithFailPolicy 显式改为 FailClosed。
func (rdb *redisClient) NewBloomFilter(key string, opts ...BloomOption) BloomFilter {
	cfg := defaultBloomConfig()
	cfg.policy = FailOpen // 过滤器默认 FailOpen：宁可放行不阻塞业务
	for _, o := range opts {
		o(&cfg)
	}

	if rdb.cap.HasBloom() {
		return &bfCmdImpl{client: rdb, key: key, cfg: cfg, policy: cfg.policy}
	}
	return newBitmapImpl(rdb, key, cfg)
}

// NewBloomFilterWithEstimate creates a BloomFilter with explicit capacity and
// false positive probability. Convenience wrapper around NewBloomFilter.
// When using the native BF.* path, this calls BF.RESERVE to pre-allocate.
func (rdb *redisClient) NewBloomFilterWithEstimate(key string, capacity int64, falsePositive float64) BloomFilter {
	return rdb.NewBloomFilter(key,
		WithCapacity(capacity),
		WithFalsePositive(falsePositive),
	)
}

// --- BF.* native implementation ---

type bfCmdImpl struct {
	client *redisClient
	key    string
	cfg    bloomConfig
	policy FailPolicy // 失效兜底策略（默认 FailOpen）
}

// fallbackBool 按策略返回布隆过滤器兜底值 + 哨兵错误：FailOpen → true
// （视为已添加/存在）；FailClosed → false。错误为 ErrRedisUnavailable 包装。
func (b *bfCmdImpl) fallbackBool(err error) (bool, error) {
	if b.policy == FailOpen {
		return true, fallbackErr(err)
	}
	return false, fallbackErr(err)
}

// fallbackBools 返回 AddMulti/ExistsMulti 的兜底切片：FailOpen → 全 true；
// FailClosed → 全 false。
func (b *bfCmdImpl) fallbackBools(n int, err error) ([]bool, error) {
	res := make([]bool, n)
	for i := range res {
		res[i] = b.policy == FailOpen
	}
	return res, fallbackErr(err)
}

func (b *bfCmdImpl) Add(ctx context.Context, item string) (bool, error) {
	added, err := b.client.BFAdd(ctx, b.key, item).Result()
	if err != nil {
		if IsUnavailable(err) {
			return b.fallbackBool(err)
		}
		return false, err
	}
	return added, nil
}

func (b *bfCmdImpl) Exists(ctx context.Context, item string) (bool, error) {
	exists, err := b.client.BFExists(ctx, b.key, item).Result()
	if err != nil {
		if IsUnavailable(err) {
			return b.fallbackBool(err)
		}
		return false, err
	}
	return exists, nil
}

func toInterfaceSlice(items []string) []interface{} {
	args := make([]interface{}, len(items))
	for i, v := range items {
		args[i] = v
	}
	return args
}

func (b *bfCmdImpl) AddMulti(ctx context.Context, items ...string) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}

	// BF.MADD returns ints: 1 if newly inserted, 0 if already present
	added, err := b.client.BFMAdd(ctx, b.key, toInterfaceSlice(items)...).Result()
	if err != nil {
		if IsUnavailable(err) {
			return b.fallbackBools(len(items), err)
		}
		return nil, err
	}

	return added, nil
}

func (b *bfCmdImpl) ExistsMulti(ctx context.Context, items ...string) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}

	results, err := b.client.BFMExists(ctx, b.key, toInterfaceSlice(items)...).Result()
	if err != nil {
		if IsUnavailable(err) {
			return b.fallbackBools(len(items), err)
		}
		return nil, err
	}
	return results, nil
}

func (b *bfCmdImpl) Info(ctx context.Context) (*BloomInfo, error) {
	info, err := b.client.BFInfo(ctx, b.key).Result()
	if err != nil {
		if IsUnavailable(err) {
			// Info 非关键：兜底返回空结构体 + 哨兵错误（errors.Is 可感知）
			return &BloomInfo{}, fallbackErr(err)
		}
		return nil, err
	}

	return &BloomInfo{
		Capacity:   info.Capacity,
		Size:       info.Size,
		NumFilters: info.Filters,
		NumItems:   info.ItemsInserted,
		Expansion:  info.ExpansionRate,
	}, nil
}

// --- Bitmap fallback implementation ---

type bitmapImpl struct {
	client   *redisClient
	key      string
	m        uint64 // bitmap size in bits
	k        uint   // number of hash functions
	capacity int64
	policy   FailPolicy // 失效兜底策略（默认 FailOpen）
}

func newBitmapImpl(client *redisClient, key string, cfg bloomConfig) *bitmapImpl {
	m := bloomBitCount(cfg.capacity, cfg.falsePositive)
	// m 奇化：与双哈希步长强制奇（h2 |= 1，见 hashs）联合消除步长与模数的
	// 公因子 2，避免位轨道减半（belt-and-braces，详见 hashs 注释）。
	m |= 1

	// Redis 字符串（位图）大小上限 512MB = 2^32-1 bit，SETBIT 偏移超出即非法。
	// capacity 合法（>0）但过大时 fail-fast（panic 先例见 MustConstraint，
	// redis.go）：静默截断会让位图语义悄悄损坏，比崩溃更危险。
	if m > math.MaxUint32 {
		panic("redis: bitmap bloom 超 Redis 位图上限 2^32-1，请降低 capacity 或部署 RedisBloom 模块")
	}

	// k 依赖 m/n，必须基于奇化后的 m 计算
	k := bloomHashCount(cfg.capacity, m)

	return &bitmapImpl{
		client:   client,
		key:      key,
		m:        m,
		k:        k,
		capacity: cfg.capacity,
		policy:   cfg.policy,
	}
}

// fallbackBool 按策略返回布隆过滤器兜底值 + 哨兵错误（与 bfCmdImpl 语义一致）。
func (b *bitmapImpl) fallbackBool(err error) (bool, error) {
	if b.policy == FailOpen {
		return true, fallbackErr(err)
	}
	return false, fallbackErr(err)
}

// bloomBitCount computes optimal bitmap size (m bits) using:
//
//	m = -n * ln(p) / (ln(2))^2
//
// 浮点值超出 uint64 可表示范围时饱和为 MaxUint64（float→uint64 溢出为
// 实现定义行为，可能截断为小值绕过 newBitmapImpl 的上限检查）；
// 饱和值同样 >2^32-1，由调用方 panic 兜住。
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
func bloomHashCount(n int64, m uint64) uint {
	k := float64(m) / float64(n) * math.Ln2
	if k < 1 {
		return 1
	}
	if k > 30 {
		return 30
	}
	return uint(math.Ceil(k))
}

// hashs returns the k bit positions for an item.
// Uses double hashing (Kirsch-Mitzenmacher): h(i) = h1 + i * h2 (mod m)
//
// 哈希源为 xxh3-128：一次 Hash128 取两个独立 64 位值（h1=Hi，h2=Lo|1）。
// h2 强制为奇数，与位图大小 m 的奇化（newBitmapImpl 中 m |= 1）联合消除
// K-M 双哈希的经典缺陷——步长与模数共享公因子 2 时，探测位被困在 h1
// 奇偶决定的半值域轨道内（轨道减半、误判率显著超标：实测旧实现
// FNV 偶步长 × 偶模数在 p=0.001 时 FPR 超标 34.95 倍）。
// 诚实声明：该修复保证步长与模数不共享因子 2，**不是完整的互质证明**
// ——m 与 h2 仍可能共享奇公因子（3、5、…），残余仅为少量轨道收缩，
// 实测误差可忽略；属 belt-and-braces 的最小代价方案。
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

// --- Lua 原子化脚本 ---
// bitmap 路径的 add/exists 通过 Lua 脚本在服务端一次往返完成：
// 原实现是 exists（k 次 GETBIT）+ k 次 SETBIT 的多命令往返，既慢又存在
// 非原子窗口（并发添加同一 item 可能重复返回"新添加"）。脚本化后单次
// EVAL 完成全部位操作，保证语义与原实现一致且原子。

var (
	// bitmapAddScript 原子添加：逐位 GETBIT 检查、SETBIT 置位。返回 1 表示
	// 调用前该 item 不可能存在（k 位中至少一位原为 0，缺位已全部置 1），
	// 对齐 BF.ADD 语义；返回 0 表示 k 位全为 1（可能存在，无缺位可补）。
	// "新增"判定基于置位之前的快照：先 GETBIT 后 SETBIT，本次调用自己置的
	// 位不参与判定，与兜底路径 addFallback 的 !exists 判据（任一位置 0 即
	// 不存在）完全一致。
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
	args := make([]interface{}, len(hashs))
	for i, p := range hashs {
		args[i] = p
	}
	return args
}

// multiPositionsArgs 组装批量脚本参数：ARGV = {k, item1 位…, item2 位…, ...}，
// 位置按 items 入参顺序展开——脚本按序输出，返回与入参严格对应。
func (b *bitmapImpl) multiPositionsArgs(items []string) []interface{} {
	args := make([]interface{}, 0, 1+len(items)*int(b.k))
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
// （单 KEYS[1]，所有命令同 key——cluster 下同 slot 合法）。
//
// 入口：记忆为 -1 时不浪费一次 EVAL 往返，直接返回 scriptFallback；
// 执行失败按 classifyLuaError 分诊迁移记忆（仅 unknown command/被禁类置 -1，
// 瞬态与数据类错误保持记忆不动）。
func (b *bitmapImpl) runBitmapScript(ctx context.Context, s *goredis.Script, args []interface{}) (*goredis.Cmd, scriptOutcome) {
	if !b.client.luaTryEval() {
		return nil, scriptFallback
	}
	cmd := s.Run(ctx, b.client, []string{b.key}, args...)
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
	cmd, outcome := b.runBitmapScript(ctx, bitmapAddScript, b.positions(item))
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

// addFallback 非原子回退：exists 检查（1 次 pipeline，k 个 GETBIT）+
// k 个 SETBIT（1 次 pipeline），往返从最坏 2k 次降为 2 次。
// 全部命令作用于同一 key，cluster 下同 slot 合法。
//
// 语义与原实现一致：Add 返回"是否新增"（先 exists 后 add）。
// **兜底路径不保证并发原子性**（检查与置位间存在窗口，并发添加同一 item
// 可能都返回"新增"）——这是 Lua 不可用时的尽力而为降级，原子性只在
// EVAL 路径成立。
func (b *bitmapImpl) addFallback(ctx context.Context, item string) (bool, error) {
	hashs := b.hashs(item)

	exists, err := b.existsFallback(ctx, item)
	if err != nil {
		if IsUnavailable(err) {
			return b.fallbackBool(err)
		}
		return false, err
	}

	pipe := b.client.Pipeline()
	for _, pos := range hashs {
		pipe.SetBit(ctx, b.key, int64(pos), 1)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		if IsUnavailable(err) {
			return b.fallbackBool(err)
		}
		return false, err
	}

	// 返回 true 表示调用前该 item 不可能存在（k 位至少一位为 0，即 !exists），
	// 与 Lua 脚本及 BF.ADD 判据一致；非"k 位全为 0"。
	return !exists, nil
}

func (b *bitmapImpl) Add(ctx context.Context, item string) (bool, error) {
	return b.add(ctx, item)
}

func (b *bitmapImpl) exists(ctx context.Context, item string) (bool, error) {
	cmd, outcome := b.runBitmapScript(ctx, bitmapExistsScript, b.positions(item))
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
// （同 key，cluster 同 slot 合法）。任一位置为 0 即不存在。
func (b *bitmapImpl) existsFallback(ctx context.Context, item string) (bool, error) {
	hashs := b.hashs(item)

	pipe := b.client.Pipeline()
	cmds := make([]*goredis.IntCmd, len(hashs))
	for i, pos := range hashs {
		cmds[i] = pipe.GetBit(ctx, b.key, int64(pos))
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

// AddMulti 批量添加：优先单次批量 Lua 脚本（1 个 KEYS[1]，ARGV 含 k 与
// n×k 个位置，返回 n 个 0/1，顺序与 items 严格对应，对齐 BF.MADD 语义）；
// 批量脚本不可用（Lua 被禁/瞬态错误）时逐条降级到现有单条路径
// （add 内部含三态分派与 fallbackBool 兜底）。
// 不实现分块：k≤30 已封顶，超大 n 的单次 Lua 阻塞代价见脚本注释声明。
func (b *bitmapImpl) AddMulti(ctx context.Context, items ...string) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}

	cmd, outcome := b.runBitmapScript(ctx, bitmapAddMultiScript, b.multiPositionsArgs(items))
	switch outcome {
	case scriptOK:
		vals, err := cmd.Int64Slice()
		if err != nil {
			return nil, err
		}
		if len(vals) != len(items) {
			return nil, fmt.Errorf("redis: bitmap AddMulti 脚本返回 %d 个结果，期望 %d", len(vals), len(items))
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
		return b.addMultiLoop(ctx, items)
	}
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

// ExistsMulti 批量存在性检查：单次批量 Lua 脚本，返回顺序与 items 严格
// 对应；失败时逐条降级到 exists（含三态分派与兜底）。空入参早返回。
func (b *bitmapImpl) ExistsMulti(ctx context.Context, items ...string) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}

	cmd, outcome := b.runBitmapScript(ctx, bitmapExistsMultiScript, b.multiPositionsArgs(items))
	switch outcome {
	case scriptOK:
		vals, err := cmd.Int64Slice()
		if err != nil {
			return nil, err
		}
		if len(vals) != len(items) {
			return nil, fmt.Errorf("redis: bitmap ExistsMulti 脚本返回 %d 个结果，期望 %d", len(vals), len(items))
		}
		result := make([]bool, len(items))
		for i, v := range vals {
			result[i] = v == 1
		}
		return result, nil
	case scriptError:
		return nil, cmd.Err()
	default:
		return b.existsMultiLoop(ctx, items)
	}
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
// 注意 BITCOUNT 为 O(bytes) 全量扫描（位图上限 512MB），属重命令，
// 仅适合低频运维查询，勿在热路径调用。
// NumFilters/Expansion 仅 BF.* 路径有意义（bitmap 无子过滤器概念），保持零值。
func (b *bitmapImpl) Info(ctx context.Context) (*BloomInfo, error) {
	strLen, err := b.client.StrLen(ctx, b.key).Result()
	if err != nil {
		if IsUnavailable(err) {
			// Info 非关键：兜底返回空结构体 + 哨兵错误（errors.Is 可感知）
			return &BloomInfo{}, fallbackErr(err)
		}
		return nil, err
	}

	bitsSet, err := b.client.BitCount(ctx, b.key, nil).Result()
	if err != nil {
		if IsUnavailable(err) {
			return &BloomInfo{}, fallbackErr(err)
		}
		return nil, err
	}

	return &BloomInfo{
		Capacity: b.capacity,
		Size:     strLen,
		NumItems: b.estimateNumItems(bitsSet),
	}, nil
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
