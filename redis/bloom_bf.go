package redis

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	goredis "github.com/redis/go-redis/v9"
)

// BF.*（RedisBloom 模块）原生命令实现——bfCmdImpl 及其专属辅助。
// 由 bloom.go 拆分而来：接口/配置留在 bloom.go，命令实现移入本文件，
// 并新增集群分片支持（路由/分组/惰性 RESERVE/Info 聚合）。
// newBFImpl 构造 BF.* 路径实现（分片参数解析与 per-shard RESERVE 闸门
// 初始化）。auto 探测单一入口，保证分片行为与 bitmap 路径完全对称。
func (rdb *redisClient) newBFImpl(key string, cfg bloomConfig) *bfCmdImpl {
	enabled, n, perShard := resolveBloomSharding(rdb.Mode(), cfg.shardCount, cfg.capacity)
	bf := &bfCmdImpl{
		client:   rdb,
		cfg:      cfg,
		policy:   cfg.policy,
		sharder:  newBloomSharder(key, enabled, n),
		perShard: perShard,
	}
	if enabled {
		// per-shard 惰性 BF.RESERVE 的一次性闸门（参照 cuckoo.go 的 once 模式）。
		// atomic.Pointer 使 Reset 能在 DEL 后逐元素换新 Once 重新武装闸门；
		// ⚠️ 零值 Load() 返回 nil，必须逐元素 Store(new(sync.Once))——漏 Store
		// 会让 reserveShard 空指针 panic。
		bf.reserves = make([]atomic.Pointer[sync.Once], n)
		for i := range bf.reserves {
			bf.reserves[i].Store(new(sync.Once))
		}
	}
	return bf
}

// --- BF.* native implementation ---

type bfCmdImpl struct {
	client *redisClient
	cfg    bloomConfig
	policy FailPolicy // 失效兜底策略（默认 FailOpen）
	// sharder 分片路由共享层（与 bitmapImpl 同一套代码，保证两路径行为一致）。
	// enabled=false（非集群或未显式开启分片）时 shardKey 恒返回 base，
	// 行为与分片化之前完全一致。
	sharder  bloomSharder
	perShard int64 // BF.RESERVE 每分片容量（ceil(总/effectiveN)）
	// reserves 每个分片键一把惰性 BF.RESERVE 闸门（仅 enabled 时分配）；
	// 并发下命中 "item exists"/"already exists" 的错误视为成功（cuckoo.go ensureReserve 同模式）。
	// 用 atomic.Pointer 承载：Reset 清空键后逐元素 Store 新 Once 重新武装
	// 闸门，与在途 reserveShard 的 Load 并发安全。
	reserves []atomic.Pointer[sync.Once]
}

// reserveShard 对第 idx 个分片键惰性执行一次 BF.RESERVE（并发只成功执行
// 一轮；once 耗尽后的后续调用不再重发）。"已存在"类错误视为初始化完成；
// 其余错误原样返回，由调用方按 Unavailable/数据类分流。
// 闸门指针经 atomic 读取：Reset 换新 Once 后本方法自动改在新 Once 上重新
// 武装一次 BF.RESERVE；与并发 Reset 交错时最多多发一条 RESERVE，其
// "already exists" 错误被下方吞错逻辑消化，无害。
func (b *bfCmdImpl) reserveShard(ctx context.Context, idx int) error {
	if !b.sharder.enabled {
		return nil // 非分片模式不预分配（历史既有行为，保持零回归）
	}
	once := b.reserves[idx].Load() // 构造点已逐元素 Store，非 nil（见 newBFImpl）
	var err error
	once.Do(func() {
		err = b.client.BFReserve(ctx, b.sharder.shardKey(idx), b.cfg.falsePositive, b.perShard).Err()
		if err != nil && (strings.Contains(err.Error(), "item exists") || strings.Contains(err.Error(), "already exists")) {
			err = nil // 过滤器已存在（并发/历史残留）：视为已初始化，武装保持
			return
		}
		if err != nil {
			// RESERVE 真实失败（网络/服务类）：sync.Once 不辨成败，Do 返回
			// 即永久燃尽——不显式解除武装的话，服务恢复后也永不再试，分片
			// 被 RedisBloom 以默认参数（capacity=100）隐式创建，容量契约
			// 静默作废（与 Reset 防的是同一类腐化，触发路径不同）。换新
			// Once 解除武装，下一次写入在本闸门上重试 RESERVE；本次调用
			// 照常返回 err（不改变调用方的 FailPolicy 兜底行为）。闭包内
			// Store 安全：当前 Do 持有者执行完自然结束，后续 Load 见到新
			// 闸门；并发重试最多多发一条 RESERVE，命中 exists 吞错无害。
			b.reserves[idx].Store(new(sync.Once))
		}
	})
	return err
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

func (b *bfCmdImpl) Add(ctx context.Context, item any) (bool, error) {
	// 先经 marshalItem 得规范字节再路由（编码格式冻结契约见 marshal.go）；
	// 编码失败（不支持类型）直接返回数据类错误，不发命令、不触发兜底。
	data, err := marshalItem(item)
	if err != nil {
		return false, err
	}
	// 集群分片：路由到 base#idx 并惰性 BF.RESERVE（standalone 恒走 base、
	// 不 reserve，行为与分片化之前一致）。
	idx := b.sharder.indexOf(data)
	key := b.sharder.shardKey(idx)
	if err := b.reserveShard(ctx, idx); err != nil {
		if IsUnavailable(err) {
			return b.fallbackBool(err)
		}
		return false, err
	}

	// 命令参数透传原始 item，由 go-redis writer 序列化——其编码与
	// marshalItem 逐字节对齐（见 marshal.go 与 parity 测试），保证
	// 路由/位哈希与服务端实际处理的字节同口径。
	added, err := b.client.BFAdd(ctx, key, item).Result()
	if err != nil {
		if IsUnavailable(err) {
			return b.fallbackBool(err)
		}
		return false, err
	}
	return added, nil
}

func (b *bfCmdImpl) Exists(ctx context.Context, item any) (bool, error) {
	data, err := marshalItem(item)
	if err != nil {
		return false, err
	}
	// 只读路径不触发 RESERVE：BF.EXISTS 对不存在的键返回 0（语义即
	// "不存在"），空分片无需预分配。
	exists, err := b.client.BFExists(ctx, b.sharder.keyFor(data), item).Result()
	if err != nil {
		if IsUnavailable(err) {
			return b.fallbackBool(err)
		}
		return false, err
	}
	return exists, nil
}

// AddMulti 批量添加（BF.MADD）。集群分片下按分片分组、单个 Pipeline 提交
// ≤effectiveN 条批量命令，结果按原始下标回填（顺序与入参严格对应）；
// 任一分片组服务不可用 → 整体按 FailPolicy 兜底（禁止混合结果），
// 数据类错误原样返回（可能已部分写入，置位幂等、重试无害）。
// 任一 item 编码失败（不支持类型）→ 整体返回数据类错误、不发命令。
func (b *bfCmdImpl) AddMulti(ctx context.Context, items ...any) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	return b.multiByShards(ctx, items, true)
}

// ExistsMulti 批量存在性检查（BF.MEXISTS），分组/回填/兜底/编码校验规则
// 与 AddMulti 一致。
func (b *bfCmdImpl) ExistsMulti(ctx context.Context, items ...any) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	return b.multiByShards(ctx, items, false)
}

// multiByShards 是 BF.* 路径批量操作的统一实现（isAdd 选 BF.MADD/BF.MEXISTS）。
// 单物理键形态（standalone 或集群退化态 n==1）保持原有的单命令直发；
// 多分片形态先对各组惰性 BF.RESERVE（一次性命令、无副作用不进管道），再
// 一个 Pipeline 提交各组 BF.MADD/BF.MEXISTS——go-redis 集群 pipeline 按
// 节点分组拆分发送，不同 slot 的键不会触发 CROSSSLOT。
func (b *bfCmdImpl) multiByShards(ctx context.Context, items []any, isAdd bool) ([]bool, error) {
	op := "BF.MEXISTS"
	if isAdd {
		op = "BF.MADD"
	}

	if !b.sharder.enabled || b.sharder.n == 1 {
		// 发送前整体校验编码：任一 item 不支持即数据类错误返回、不发命令
		// （与分片态 group 的路由编码同口径，保证两形态错误语义一致）。
		for _, item := range items {
			if _, err := marshalItem(item); err != nil {
				return nil, err
			}
		}
		key := b.sharder.shardKey(0) // standalone 即 base；集群退化态为 base#0
		if err := b.reserveShard(ctx, 0); err != nil {
			if IsUnavailable(err) {
				return b.fallbackBools(len(items), err)
			}
			return nil, err
		}
		var added []bool
		var err error
		if isAdd {
			added, err = b.client.BFMAdd(ctx, key, items...).Result()
		} else {
			added, err = b.client.BFMExists(ctx, key, items...).Result()
		}
		if err != nil {
			if IsUnavailable(err) {
				return b.fallbackBools(len(items), err)
			}
			return nil, err
		}
		return added, nil
	}

	// group 内部先统一 marshalItem 得路由字节：任一 item 编码失败即返回
	// 数据类错误，不发任何命令。
	groups, err := b.sharder.group(items)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if err := b.reserveShard(ctx, g.idx); err != nil {
			if IsUnavailable(err) {
				return b.fallbackBools(len(items), err) // 任一分片不可用 → 整体兜底
			}
			return nil, err
		}
	}

	pipe := b.client.Pipeline()
	cmds := make([]*goredis.BoolSliceCmd, len(groups))
	for i, g := range groups {
		if isAdd {
			cmds[i] = pipe.BFMAdd(ctx, g.key, g.items...)
		} else {
			cmds[i] = pipe.BFMExists(ctx, g.key, g.items...)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && IsUnavailable(err) {
		return b.fallbackBools(len(items), err)
	}

	result := make([]bool, len(items))
	for i, g := range groups {
		vals, err := cmds[i].Result()
		if err != nil {
			if IsUnavailable(err) {
				return b.fallbackBools(len(items), err)
			}
			// 数据类错误原样返回：其他分片可能已写入——布隆置位幂等，
			// 整体重试无数据危害（见 AddMulti 接口注释）。
			return nil, err
		}
		if len(vals) != len(g.items) {
			return nil, fmt.Errorf("redis: %s 分片 %d 返回 %d 个结果，期望 %d", op, g.idx, len(vals), len(g.items))
		}
		for j, v := range vals {
			result[g.srcIdx[j]] = v // 按原始下标回填，顺序与入参对应
		}
	}
	return result, nil
}

// Info 聚合全部分片键的 BF.INFO。
// ⚠️ 成本警告（分片后加重）：分片态对每个分片键各发一次 BF.INFO——往返
// ×effectiveN，Info 本是重命令，放大后仅适合更低频的运维查询，严禁热路径。
// 空分片（从未初始化）在分片态把 "not found" 归一为零值分片继续聚合，
// 不再整体报错；standalone 维持历史报错行为（零回归）。
// Capacity/Size/NumFilters/NumItems 逐分片求和（BF 路径的 ItemsInserted 是
// 精确计数，线性可加）；ExpansionRate 是 RESERVE 配置常量、各分片一致，
// 取第一个分片。
func (b *bfCmdImpl) Info(ctx context.Context) (*BloomInfo, error) {
	agg := &BloomInfo{}
	emptyShardOK := b.sharder.enabled
	haveExpansion := false // Expansion 取**首个成功分片**（首片可能是空分片）
	for _, key := range b.sharder.allKeys() {
		info, err := b.client.BFInfo(ctx, key).Result()
		if err != nil {
			if IsUnavailable(err) {
				// Info 非关键：兜底返回空结构体 + 哨兵错误（errors.Is 可感知）
				return &BloomInfo{}, fallbackErr(err)
			}
			if emptyShardOK && strings.Contains(err.Error(), "not found") {
				continue // 零值分片：该分片从未写入过，贡献 0
			}
			return nil, err
		}
		agg.Capacity += info.Capacity
		agg.Size += info.Size
		agg.NumFilters += info.Filters
		agg.NumItems += info.ItemsInserted
		if !haveExpansion {
			agg.Expansion = info.ExpansionRate
			haveExpansion = true
		}
	}
	return agg, nil
}

// Card 逐分片 BF.CARD 求和（for-allKeys 惯例与 Info 一致；去重口径与
// 误差声明见 BloomFilter.Card 接口注释）。与 BF.INFO 不同：BF.CARD 对
// 不存在的键返回 0 不报错，空分片无需 "not found" 特判归一。
// 服务不可用返回 (0, fallbackErr)——观测类方法不随 FailPolicy 分叉；
// 数据类错误原样返回（对齐 Info 的既有分流）。
func (b *bfCmdImpl) Card(ctx context.Context) (int64, error) {
	var sum int64
	for _, key := range b.sharder.allKeys() {
		n, err := b.client.BFCard(ctx, key).Result()
		if err != nil {
			if IsUnavailable(err) {
				return 0, fallbackErr(err)
			}
			return 0, err
		}
		sum += n
	}
	return sum, nil
}

// Reset 清空全部物理键（DEL，共享层见 resetBloomKeys）并**无条件**复位
// 惰性 BF.RESERVE 闸门——后续首次写入重新执行 BF.RESERVE，容量契约
// （perShard）不因重建而失效。语义与限制见 BloomFilter.Reset 接口注释。
//
// 闸门复位放在 defer：不区分 DEL 成败一律换新 sync.Once。若"仅 DEL 成功
// 才复位"，DEL 实际执行成功但客户端收到网络错误的场景会让燃尽的 once
// 残留——后续 Add 不再 RESERVE，键被 RedisBloom 以默认参数
// （capacity=100）自动重建，容量契约静默作废；而无条件复位导致的并发
// 双 RESERVE 会被 reserveShard 既有 "already exists" 吞错逻辑消化，无害。
func (b *bfCmdImpl) Reset(ctx context.Context) error {
	defer func() {
		for i := range b.reserves {
			b.reserves[i].Store(new(sync.Once))
		}
	}()
	return resetBloomKeys(ctx, b.client, b.sharder)
}
