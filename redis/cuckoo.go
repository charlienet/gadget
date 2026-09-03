package redis

import (
	"context"
	"strings"
	"sync"

	goredis "github.com/redis/go-redis/v9"
)

// CuckooInfo 包含布谷鸟过滤器的元数据（模块版对应 CF.INFO 输出；
// 回退版为占用统计，未涉及的字段为零值）。
type CuckooInfo struct {
	Size          int64 // 过滤器大小（字节）
	NumBuckets    int64 // 桶数量
	NumFilters    int64 // 过滤器数量
	NumItems      int64 // 已插入元素数
	NumDeletes    int64 // 已删除元素数
	Expansion     int64 // 扩容因子
	BucketSize    int64 // 桶大小
	MaxIterations int64 // 最大踢出迭代次数
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
type cuckooFilterImpl interface {
	Add(ctx context.Context, item string) (bool, error)
	Exists(ctx context.Context, item string) (bool, error)
	Del(ctx context.Context, item string) (bool, error)
	Info(ctx context.Context) (*CuckooInfo, error)
}

// CuckooFilter 是布谷鸟过滤器的统一门面，按服务器能力自动分派实现：
//   - 服务器加载了 RedisBloom 的 cuckoo 模块 → cfCmdImpl（原生 CF.* 命令）
//   - 未加载 → hashImpl（Hash + Lua 回退，普通 Redis 即可运行，无模块依赖）
//
// 与 BloomFilter 不同，布谷鸟过滤器支持删除（Del），且误判率更低。
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
func (rdb *redisClient) NewCuckooFilter(key string, opts ...CuckooOption) *CuckooFilter {
	cfg := defaultCuckooConfig()
	cfg.policy = FailOpen // 过滤器默认 FailOpen：宁可放行不阻塞业务
	for _, o := range opts {
		o(&cfg)
	}

	if rdb.cap.HasCuckoo() {
		return &CuckooFilter{impl: &cfCmdImpl{client: rdb, key: key, cfg: cfg}, policy: cfg.policy}
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

// Add 向过滤器添加一个元素，返回是否新增（false 表示元素已存在）。
// Redis 服务失效时按兜底策略：FailOpen → (true, nil)；FailClosed → (false, nil)。
func (cf *CuckooFilter) Add(ctx context.Context, item string) (bool, error) {
	added, err := cf.impl.Add(ctx, item)
	if err != nil && IsUnavailable(err) {
		return cf.fallbackBool(err)
	}
	return added, err
}

// Exists 检查元素是否可能存在于过滤器（布谷鸟过滤器无假阴性，可能有假阳性）。
// Redis 服务失效时按兜底策略：FailOpen → (true, nil)（视为存在，防穿透失效
// 但放行业务）；FailClosed → (false, nil)。
func (cf *CuckooFilter) Exists(ctx context.Context, item string) (bool, error) {
	exists, err := cf.impl.Exists(ctx, item)
	if err != nil && IsUnavailable(err) {
		return cf.fallbackBool(err)
	}
	return exists, err
}

// Del 从过滤器删除一个元素。注意与 BF 不同：CF.DEL 返回是否删除成功
// （元素不存在或删除导致桶耗尽时返回 false）。
// Redis 服务失效时按兜底策略：FailOpen → (true, nil)（视为删除成功）；
// FailClosed → (false, nil)。
func (cf *CuckooFilter) Del(ctx context.Context, item string) (bool, error) {
	deleted, err := cf.impl.Del(ctx, item)
	if err != nil && IsUnavailable(err) {
		return cf.fallbackBool(err)
	}
	return deleted, err
}

// Info 返回过滤器的元数据。
// Redis 服务失效时按兜底策略：FailOpen → 空结构体 nil；FailClosed → 返回错误。
func (cf *CuckooFilter) Info(ctx context.Context) (*CuckooInfo, error) {
	info, err := cf.impl.Info(ctx)
	if err != nil && IsUnavailable(err) {
		// Info 非关键：兜底返回空结构体 + 哨兵错误（errors.Is 可感知）
		return &CuckooInfo{}, fallbackErr(err)
	}
	return info, err
}

// ---------------------------------------------------------------------------
// 模块版实现：原生 CF.* 命令（依赖 RedisBloom cuckoo 模块）
// ---------------------------------------------------------------------------

type cfCmdImpl struct {
	client *redisClient
	key    string
	cfg    cuckooConfig
	once   sync.Once // 惰性 CF.RESERVE 只执行一次
}

// ensureReserve 在配置了容量时对过滤器执行一次 CF.RESERVE 预分配。
// 对已存在的过滤器（CF.RESERVE 报 "item exists"）容错忽略。
func (cf *cfCmdImpl) ensureReserve(ctx context.Context) error {
	if cf.cfg.capacity <= 0 {
		return nil
	}

	var err error
	cf.once.Do(func() {
		opt := &goredis.CFReserveOptions{
			Capacity:      cf.cfg.capacity,
			BucketSize:    cf.cfg.bucketSize,
			MaxIterations: cf.cfg.maxIterations,
			Expansion:     cf.cfg.expansion,
		}
		err = cf.client.CFReserveWithArgs(ctx, cf.key, opt).Err()
		if err != nil && strings.Contains(err.Error(), "exists") {
			err = nil // 过滤器已存在：视为已初始化
		}
	})

	return err
}

func (cf *cfCmdImpl) Add(ctx context.Context, item string) (bool, error) {
	if err := cf.ensureReserve(ctx); err != nil {
		return false, err
	}
	return cf.client.CFAdd(ctx, cf.key, item).Result()
}

func (cf *cfCmdImpl) Exists(ctx context.Context, item string) (bool, error) {
	return cf.client.CFExists(ctx, cf.key, item).Result()
}

func (cf *cfCmdImpl) Del(ctx context.Context, item string) (bool, error) {
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
// 哈希（Go 侧计算候选桶，Lua 侧计算指纹哈希，公式一致）：
//   - fp = fnv1a(item) & 0xFFFF（2 字节指纹，0 时取 1）——**2 字节空间
//     大幅降低指纹冲突**：1 字节（255 种）在元素多时冲突严重，驱逐链无法
//     区分同指纹的不同元素（owner），会把元素指纹移入"另一同指纹元素的
//     候选桶"造成放错（方向错误假阴性）；2 字节（65535 种）冲突率极低，
//     驱逐链的 alternate 恒为该指纹 owner 的候选桶。
//   - i1 = fnv1a(item) % numBuckets
//   - i2 = (i1 + h(fp)) % numBuckets —— 模加候选桶关系（Lua 5.1 无位运算，
//     XOR 需算术模拟；模加可直接计算）
//   - h(fp) = (fp × 2654435761) % 2^32 % numBuckets —— 乘法哈希（模拟 32 位
//     回绕；fp < 65536 时乘积 < 2^53，Lua double 可精确表示）
//
// 驱逐机制（Add Lua 脚本）：两候选桶均满时确定性扰动选一个槽位踢出旧指纹。
// 被踢指纹的 alternate 桶由**方向位**决定：方向 0（本桶是 i1）→ 去
// (cur + h(fp)) % n；方向 1（本桶是 i2）→ 去 (cur - h(fp)) % n，且方向取反。
// 该不变量保证：驱逐链上每个指纹始终位于其两个候选桶之一——**Add 返回 true
// 的元素 Exists 必命中（无假阴性）**；仅当驱逐链超过 maxIterations（桶过载）
// 时该次 Add 返回 false（与 CF.ADD 满时返回 false 的语义对齐），链尾指纹
// 可能被挤出（cuckoo 超载的正常行为：元素被驱逐丢失，非方向错误）。
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

	numBuckets := capacity / bucketSize
	if numBuckets < 1 {
		numBuckets = 1
	}

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
func (h *hashImpl) cuckooHashs(item string) (fp int64, i1, i2 int64) {
	h1 := fnv1a(item)
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

func (h *hashImpl) Add(ctx context.Context, item string) (bool, error) {
	fp, i1, i2 := h.cuckooHashs(item)
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

func (h *hashImpl) Exists(ctx context.Context, item string) (bool, error) {
	fp, i1, i2 := h.cuckooHashs(item)
	n, err := cuckooExistsScript.Run(ctx, h.client, []string{h.key}, fp, i1, i2, h.bucketSize).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
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

func (h *hashImpl) Del(ctx context.Context, item string) (bool, error) {
	fp, i1, i2 := h.cuckooHashs(item)
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
