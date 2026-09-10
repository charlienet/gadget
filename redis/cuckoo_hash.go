package redis

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeebo/xxh3"
)

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
