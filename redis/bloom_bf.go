package redis

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

// BF.*（RedisBloom 模块）原生命令实现——bfCmdImpl 及其专属辅助。
// 接口/配置定义在 bloom.go，集群分片共享层在 bloom_shard.go；本文件
// 实现命令路由、分组批量、构造期连接（connectAll）与 Info 聚合。
// newBFImpl 仅做分片参数解析与结构装配；建键/校验由工厂经 connectAll
// 同步执行。auto 探测单一入口，保证分片行为与 bitmap 路径完全对称。
func (rdb *redisClient) newBFImpl(key string, cfg bloomConfig) *bfCmdImpl {
	enabled, n, perShard := resolveBloomSharding(rdb.Mode(), cfg.shardCount, cfg.capacity)
	return &bfCmdImpl{
		client:   rdb,
		cfg:      cfg,
		policy:   cfg.policy,
		sharder:  newBloomSharder(key, enabled, n),
		perShard: perShard,
	}
}

// --- BF.* native implementation ---

type bfCmdImpl struct {
	client *redisClient
	cfg    bloomConfig
	policy FailPolicy // 失效兜底策略（默认 FailOpen）
	// sharder 分片路由共享层（与 bitmapImpl 同一套代码，保证两路径行为一致）。
	// enabled=false（非集群或未显式开启分片）时 shardKey 恒返回 base，
	// 单键直达。
	sharder  bloomSharder
	perShard int64 // BF.RESERVE 每分片容量（ceil(总/effectiveN)）
}

// connectKey 对单个物理键执行 BF.RESERVE <fp> <perShard>，三态结果：
// 成功=键按当前配置新建；"item exists"/"already exists" 错误=键为既有
// 真 bloom 过滤器、复用（exists 类错误是唯一复用判据，键类型校验随
// RESERVE 完成）；其余错误（WRONGTYPE、模块间类型互撞等）原样返回、
// 不触碰键。falsePositive 无法从服务端回读核验（BF.INFO 不回该字段），
// 复用路径的 fp 一致性属能力边界外。
func (b *bfCmdImpl) connectKey(ctx context.Context, key string) error {
	return b.client.BFReserve(ctx, key, b.cfg.falsePositive, b.perShard).Err()
}

// classifyConnectErr 统一逐键 BF.RESERVE 结果的三态分流：新建成功与复用
// （exists 吞错）均返回 nil；其余错误附键名上抛（数据类）或包
// ErrRedisUnavailable 哨兵（Unavailable 类）。
func (b *bfCmdImpl) classifyConnectErr(key string, err error) error {
	switch {
	case err == nil:
		return nil // 新建：键已按当前配置建立
	case isItemExistsText(err):
		return nil // 复用：既有真 bloom 键（参数不改写；fp 不可核验）
	case IsUnavailable(err):
		return fallbackErr(err)
	default:
		// 非 bloom 类型 / 模块类型互撞等：fail-loud，键未被修改
		return fmt.Errorf("redis: bloom BF.RESERVE %s: %w", key, err)
	}
}

// connectAll 同步连接全部物理键（多分片经单个 Pipeline 一批提交，
// 逐键按 classifyConnectErr 三态分流）；不随 FailPolicy 兜底——
// 构造/重置的失败必须可见。
func (b *bfCmdImpl) connectAll(ctx context.Context) error {
	if !b.sharder.enabled || b.sharder.n == 1 {
		key := b.sharder.shardKey(0)
		return b.classifyConnectErr(key, b.connectKey(ctx, key))
	}
	pipe := b.client.Pipeline()
	cmds := make([]*goredis.StatusCmd, b.sharder.n)
	for idx := range b.sharder.n {
		cmds[idx] = pipe.BFReserve(ctx, b.sharder.shardKey(idx), b.cfg.falsePositive, b.perShard)
	}
	if _, err := pipe.Exec(ctx); err != nil && IsUnavailable(err) {
		return fallbackErr(err)
	}
	for idx, cmd := range cmds {
		key := b.sharder.shardKey(idx)
		if err := b.classifyConnectErr(key, cmd.Err()); err != nil {
			return err
		}
	}
	return nil
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
	idx := b.sharder.indexOf(data)
	key := b.sharder.shardKey(idx)

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
// 多分片形态按分组经**单个 Pipeline** 提交各组 BF.MADD/BF.MEXISTS——
// go-redis 集群 pipeline 按节点分组拆分发送，不同 slot 的键不会触发
// CROSSSLOT。
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
// 不再整体报错；standalone 对不存在的键报错返回。
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
			if emptyShardOK && isNotFoundByText(err) {
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

// Reset 清空全部物理键（DEL，共享层见 resetBloomKeys）后立即经 connectAll
// 按当前配置同步重建——返回成功即键已就绪，容量契约（perShard）不因重建
// 而失效；失败如实返回（无闸门、无惰性自愈，键可能处于已清空未重建态），
// 重试 Reset 幂等（DEL 与 RESERVE-复用皆幂等）。错误分流同工厂：
// Unavailable 包哨兵、数据类原样返回。语义与限制见 BloomFilter.Reset
// 接口注释。
func (b *bfCmdImpl) Reset(ctx context.Context) error {
	if err := resetBloomKeys(ctx, b.client, b.sharder); err != nil {
		return err
	}
	return b.connectAll(ctx)
}
