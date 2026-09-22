package redis

import (
	"context"
	"fmt"
	"strings"

	goredis "github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// 模块版实现：原生 CF.* 命令（依赖 RedisBloom cuckoo 模块）
// ---------------------------------------------------------------------------

type cfCmdImpl struct {
	client *redisClient
	key    string
	cfg    cuckooConfig
}

// defaultCFReserveCapacity 是未显式传 WithCuckooCapacity 时 CF.RESERVE 的
// 预建容量（分配契约：键在构造期即以该容量在服务端建立）。
const defaultCFReserveCapacity int64 = 1_000_000

// connectAll 同步连接物理键：CF.RESERVE <capacity> [BUCKETSIZE
// <bucketSize>] [MAXITERATIONS <maxIterations>] [EXPANSION <expansion>]
// （未显式传容量时取 defaultCFReserveCapacity；bucketSize 等零值字段
// goredis 不附带，服务端用自身默认）。三态分流：
//   - RESERVE 成功 → 键按当前配置新建；
//   - "item exists"/"already exists" → 既有真 CF 键：复用，并按
//     verifyExisting 比对显式传入的参数；
//   - 其余错误（WRONGTYPE、模块间类型互撞等）附键名上抛，不触碰键。
//
// Unavailable 类包 ErrRedisUnavailable 哨兵；不随 FailPolicy 兜底——
// 构造/重置的失败必须可见。
func (cf *cfCmdImpl) connectAll(ctx context.Context) error {
	capacity := cf.cfg.capacity
	if capacity <= 0 {
		capacity = defaultCFReserveCapacity
	}
	opt := &goredis.CFReserveOptions{
		Capacity:      capacity,
		BucketSize:    cf.cfg.bucketSize,
		MaxIterations: cf.cfg.maxIterations,
		Expansion:     cf.cfg.expansion,
	}
	err := cf.client.CFReserveWithArgs(ctx, cf.key, opt).Err()
	switch {
	case err == nil:
		return nil // 新建完成
	case isCFKeyExistsErr(err):
		return cf.verifyExisting(ctx) // 复用：按声明口径校验（见 verifyExisting）
	case IsUnavailable(err):
		return fallbackErr(err)
	default:
		// 非 CF 类型 / 模块类型互撞（如 BF 键）等：fail-loud，键未被修改
		return fmt.Errorf("redis: cuckoo CF.RESERVE %s: %w", cf.key, err)
	}
}

// isCFKeyExistsErr 是 CF.* 路径的"键已存在"复用判据。
func isCFKeyExistsErr(err error) bool {
	return isItemExistsText(err)
}

// verifyExisting 对复用的既有 CF 键按声明口径校验：capacity **恒比对**
// ——未显式传 WithCuckooCapacity 时以默认 1000000 承载声明——以服务端
// 总槽位（NumBuckets×BucketSize）不小于期望值判定（模块按容量推导桶数
// 并向上取 2 的幂，等值比对必然误报）；bucketSize、maxIterations 仅在
// 显式传入（>0）时等值比对。不符 → 数据类 layout mismatch、键未被
// 修改，Reset 或换键解决。expansion 不比对——CF.INFO 回读的
// ExpansionRate 与服务端版本默认值联动，非请求参数恒等回读，等值判定
// 会误伤复用路径。
func (cf *cfCmdImpl) verifyExisting(ctx context.Context) error {
	capacity := cf.cfg.capacity
	if capacity <= 0 {
		capacity = defaultCFReserveCapacity
	}
	info, err := cf.client.CFInfo(ctx, cf.key).Result()
	if err != nil {
		if IsUnavailable(err) {
			return fallbackErr(err)
		}
		return fmt.Errorf("redis: cuckoo CF.INFO %s: %w", cf.key, err)
	}
	var problems []string
	if cf.cfg.bucketSize > 0 && info.BucketSize != cf.cfg.bucketSize {
		problems = append(problems, fmt.Sprintf("bucket_size server %d, want %d", info.BucketSize, cf.cfg.bucketSize))
	}
	if cf.cfg.maxIterations > 0 && info.MaxIteration != cf.cfg.maxIterations {
		problems = append(problems, fmt.Sprintf("max_iterations server %d, want %d", info.MaxIteration, cf.cfg.maxIterations))
	}
	// 容量口径：服务端未回 capacity 字段，总槽位=NumBuckets×BucketSize；
	// 既有键容纳能力低于期望即不符。
	if capacity > 0 && info.NumBuckets*info.BucketSize < capacity {
		problems = append(problems, fmt.Sprintf("capacity server %d, want >= %d", info.NumBuckets*info.BucketSize, capacity))
	}
	if len(problems) > 0 {
		return fmt.Errorf("redis: cuckoo CF %s: layout mismatch: %s；Reset 或换键", cf.key, strings.Join(problems, ", "))
	}
	return nil
}

// Add/Exists/Del 把原始 item 透传给 CF.* 模块命令，由 go-redis writer
// 序列化（与回退版 marshalItem 的字节口径一致，见 marshal.go）。
func (cf *cfCmdImpl) Add(ctx context.Context, item any) (bool, error) {
	return cf.client.CFAdd(ctx, cf.key, item).Result()
}

func (cf *cfCmdImpl) Exists(ctx context.Context, item any) (bool, error) {
	return cf.client.CFExists(ctx, cf.key, item).Result()
}

// ExistsMulti 单条 CF.MEXISTS 批量检查，结果与入参顺序一一对应。
// 只读路径不触发连接建立（与 Exists 现状一致：真机实测
// CF.MEXISTS/CF.EXISTS 对不存在的键宽容返回全 false、无错误，预建
// 无收益）。item 直发不做 marshalItem 预编码校验——与单条 Exists
// 同口径（go-redis writer 对不可序列化类型 panic 属开发者错误；
// 回退版的预校验差异见 hashImpl）。
func (cf *cfCmdImpl) ExistsMulti(ctx context.Context, items ...any) ([]bool, error) {
	res, err := cf.client.CFMExists(ctx, cf.key, items...).Result()
	if err != nil {
		return res, err
	}
	if len(res) != len(items) {
		return nil, fmt.Errorf("redis: CF.MEXISTS 返回 %d 个结果，期望 %d", len(res), len(items))
	}
	return res, nil
}

// Count 直发 CF.COUNT（只读，不触发建立动作）。键不存在时
// RedisBloom 返回 0 而非报错。返回值为出现次数估计，可能因指纹碰撞
// 高估；模块版可取任意值（CF.ADD 多重集语义），见门面 Count godoc。
func (cf *cfCmdImpl) Count(ctx context.Context, item any) (int64, error) {
	return cf.client.CFCount(ctx, cf.key, item).Result()
}

// AddNX 直发 CF.ADDNX：元素已存在则不插入。CF.ADDNX 返回 0/1
// （BoolCmd，无 -1 形态——-1 是 CF.INSERTNX 的返回），true 表示实际
// 插入。
func (cf *cfCmdImpl) AddNX(ctx context.Context, item any) (bool, error) {
	return cf.client.CFAddNX(ctx, cf.key, item).Result()
}

// AddMulti 批量插入走单条 CF.INSERT：options 恒置 nil——不带 CAPACITY/
// NOCREATE，参数分配统一经构造期 connectAll 的 CF.RESERVE 通道（避免
// RESERVE 与 INSERT 双通道配置语义分裂，架构定稿）。结果 1/-1 由
// BoolSliceCmd 归一为 true/false（false=该元素插入失败，桶满/驱逐超限）。
// item 不做 marshalItem 预编码校验、直发 writer 序列化（与单条 Add 同
// 口径；回退版的整体前置校验差异见 hashImpl.AddMulti）。
func (cf *cfCmdImpl) AddMulti(ctx context.Context, items ...any) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	res, err := cf.client.CFInsert(ctx, cf.key, nil, items...).Result()
	if err != nil {
		return res, err
	}
	if len(res) != len(items) {
		return nil, fmt.Errorf("redis: CF.INSERT 返回 %d 个结果，期望 %d", len(res), len(items))
	}
	return res, nil
}

func (cf *cfCmdImpl) Del(ctx context.Context, item any) (bool, error) {
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

// Reset 删除 CF.* 键（DEL）后立即经 connectAll 按当前配置同步重建——
// 返回成功即键已就绪；失败如实返回（键可能处于已清空未重建态），重试
// Reset 幂等（DEL 与 RESERVE/复用三态皆幂等）。键不存在时 DEL 返回 0、
// 无错误。错误分流同构造期（connectAll）。
func (cf *cfCmdImpl) Reset(ctx context.Context) error {
	if err := cf.client.Del(ctx, cf.key).Err(); err != nil {
		if IsUnavailable(err) {
			return fallbackErr(err)
		}
		return err
	}
	return cf.connectAll(ctx)
}
