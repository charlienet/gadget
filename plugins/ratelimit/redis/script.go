package redis

import (
	goredis "github.com/redis/go-redis/v9"
)

// Wholesale 的 mode 参数值（ARGV[5]），与 ratelimit.GrantMode 的 iota 序号
// 一一对应，由 int(mode) 直接转换传入：
//
//	modeBestEffort   = 0 // ratelimit.GrantBestEffort：能授多少授多少
//	modeAllOrNothing = 1 // ratelimit.GrantAllOrNothing：不足额拒绝且不扣减
const (
	modeBestEffort   = 0
	modeAllOrNothing = 1
)

// wholesaleScript 是 GCRA 令牌桶批发脚本：以 gadget/redis 模块的
// tokenBucketAtMostScript（redis/ratelimit.go:71-114，公共 API 冻结、原脚本
// 不动）为基础复制改造，三处有意差异：
//
//  1. ARGV[1]=Spec.Burst——原调用 burst 恒等于 rate（redis/ratelimit.go:283-284），
//     改造版 burst 与 rate 解耦、由 Spec 显式下发；
//  2. BestEffort 裁剪分支（mode=0 且 remaining<cost）先 math.floor 再推进 TAT
//     并返回（N5）——原脚本以小数 cost 推进，Lua number→RESP integer 向下
//     截断，每次部分授予蒸发 ≤1 令牌；改造版保证"扣减量 == 返回量"；
//  3. 新增 AllOrNothing 分支（mode=1 且 remaining<cost）：直接拒绝返回
//     {0, remaining, emission_interval×cost - diff, reset_after}，不推进 TAT、
//     不执行 SET（修 H3 配额蒸发）。该分支**前置于 remaining<1 拒绝分支**
//     （N-E）：cost >= 1 时 remaining<cost 已覆盖 remaining<1 的情形，
//     retry_after 统一用"凑够 cost 个"的精确公式，避免 cost>1 时按
//     "凑够 1 个"低估导致 Wait 提前唤醒空转。拒绝时旧 key 的 EX=reset_after
//     恰在桶回满时刻过期；过期后 GET nil→'0'→tat=now→满桶重建，语义等价
//     无跳变；首次请求 tat=now→remaining≥Burst≥cost，无冷启动死角。
//
// 其余（redis.call('TIME') 取时 + jan_1_2017 基准偏移 + replicate_commands +
// SET EX ceil(reset_after) 的闲置自动过期）逐字保留原脚本。
//
// 返回 {实际消耗 cost, remaining, retry_after(秒字符串), reset_after(秒字符串)}，
// 放行时 retry_after 为 "-1" 占位，与原脚本约定一致。
//
// 同源提醒：本脚本与 redis 模块 tokenBucketAtMostScript 内容同源，修改任何一方
// 的公共语义（TIME 基准、返回结构、EX 策略）时必须同步评估另一方。
var wholesaleScript = goredis.NewScript(`
-- this script has side-effects, so it requires replicate commands mode
redis.replicate_commands()

local rate_limit_key = KEYS[1]
local burst = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local period = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])
local mode = tonumber(ARGV[5])

local emission_interval = period / rate
local burst_offset = emission_interval * burst

local jan_1_2017 = 1483228800
local now = redis.call('TIME')
now = (now[1] - jan_1_2017) + (now[2] / 1000000)

local tat = tonumber(redis.call('GET', rate_limit_key) or '0')
tat = math.max(tat, now)

local diff = now - (tat - burst_offset)
local remaining = diff / emission_interval

if mode == 1 and remaining < cost then
	-- AllOrNothing：不足额拒绝，不推进 TAT、不 SET（防配额蒸发）。
	-- N-E：本分支前置于 remaining < 1——cost >= 1 时 remaining < cost 已覆盖
	-- remaining < 1，retry_after 统一为"凑够 cost 个"的精确公式
	-- （cost=1 特例即 emission_interval - diff），避免 cost > 1 时按
	-- "凑够 1 个"低估唤醒时机。
	local reset_after = tat - now
	local retry_after = emission_interval * cost - diff
	return {0, remaining, tostring(retry_after), tostring(reset_after)}
end

if remaining < 1 then
	local reset_after = tat - now
	local retry_after = emission_interval - diff
	return {0, 0, tostring(retry_after), tostring(reset_after)}
end

if remaining < cost then
	-- BestEffort 裁剪（N5）：按 floor(remaining) 整数授予，扣减量 == 返回量。
	cost = math.floor(remaining)
	remaining = 0
else
	remaining = remaining - cost
end

local new_tat = tat + emission_interval * cost

local reset_after = new_tat - now
if reset_after > 0 then
	redis.call('SET', rate_limit_key, new_tat, 'EX', math.ceil(reset_after))
end
return {cost, remaining, tostring(-1), tostring(reset_after)}
`)
