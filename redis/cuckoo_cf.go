package redis

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	goredis "github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// 模块版实现：原生 CF.* 命令（依赖 RedisBloom cuckoo 模块）
// ---------------------------------------------------------------------------

type cfCmdImpl struct {
	client *redisClient
	key    string
	cfg    cuckooConfig
	// once 是惰性 CF.RESERVE 闸门（每代只执行一次）。用 atomic.Pointer 包
	// sync.Once 以便 Reset 整键销毁后复位：DEL 之后 Store 一个新 sync.Once，
	// 后续 Add 重新触发 CF.RESERVE，避免 once 燃尽残留导致过滤器被模块默认
	// 参数（capacity=100、bucketSize=2、maxIterations=20）隐式重建、静默
	// 作废 WithCuckooCapacity/WithBucketSize/WithMaxIterations/WithExpansion。
	// 构造点必须显式 Store(new(sync.Once))——atomic.Pointer 零值 Load 返回 nil。
	once atomic.Pointer[sync.Once]
}

// ensureReserve 在配置了容量时对过滤器执行一次 CF.RESERVE 预分配。
// 对已存在的过滤器（CF.RESERVE 报 "item exists"/"already exists"）容错忽略
// （维持武装，闸门视为已消费）。
// 其他错误（网络失败、WRONGTYPE 等）时解除武装：Store 一个新 sync.Once，
// 下次 Add 重试 CF.RESERVE。sync.Once 不辨闭包成败——若不解除武装，一次
// 瞬态网络失败会永久燃尽闸门，过滤器随后被模块默认参数隐式重建，
// With* 配置静默作废（与 Reset 的无条件复位同纪律的两半：一个管销毁、
// 一个管失败）。
func (cf *cfCmdImpl) ensureReserve(ctx context.Context) error {
	if cf.cfg.capacity <= 0 {
		return nil
	}

	var err error
	cf.once.Load().Do(func() {
		opt := &goredis.CFReserveOptions{
			Capacity:      cf.cfg.capacity,
			BucketSize:    cf.cfg.bucketSize,
			MaxIterations: cf.cfg.maxIterations,
			Expansion:     cf.cfg.expansion,
		}
		err = cf.client.CFReserveWithArgs(ctx, cf.key, opt).Err()
		if err != nil && (strings.Contains(err.Error(), "item exists") || strings.Contains(err.Error(), "already exists")) {
			err = nil // 过滤器已存在：视为已初始化
		}
		if err != nil {
			cf.once.Store(new(sync.Once)) // 失败解除武装：本代闸门弃用，下次重试
		}
	})

	return err
}

// Add/Exists/Del 把原始 item 透传给 CF.* 模块命令，由 go-redis writer
// 序列化（与回退版 marshalItem 的字节口径一致，见 marshal.go）。
func (cf *cfCmdImpl) Add(ctx context.Context, item any) (bool, error) {
	if err := cf.ensureReserve(ctx); err != nil {
		return false, err
	}
	return cf.client.CFAdd(ctx, cf.key, item).Result()
}

func (cf *cfCmdImpl) Exists(ctx context.Context, item any) (bool, error) {
	return cf.client.CFExists(ctx, cf.key, item).Result()
}

// ExistsMulti 单条 CF.MEXISTS 批量检查，结果与入参顺序一一对应。
// 只读路径不触发 ensureReserve（与 Exists 现状一致：真机实测
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

// Count 直发 CF.COUNT（只读，不触发 ensureReserve）。键不存在时
// RedisBloom 返回 0 而非报错。返回值为出现次数估计，可能因指纹碰撞
// 高估；模块版可取任意值（CF.ADD 多重集语义），见门面 Count godoc。
func (cf *cfCmdImpl) Count(ctx context.Context, item any) (int64, error) {
	return cf.client.CFCount(ctx, cf.key, item).Result()
}

// AddNX 直发 CF.ADDNX：元素已存在则不插入。CF.ADDNX 返回 0/1
// （BoolCmd，无 -1 形态——-1 是 CF.INSERTNX 的返回），true 表示实际
// 插入。写路径先 ensureReserve（与 Add 相同）。
func (cf *cfCmdImpl) AddNX(ctx context.Context, item any) (bool, error) {
	if err := cf.ensureReserve(ctx); err != nil {
		return false, err
	}
	return cf.client.CFAddNX(ctx, cf.key, item).Result()
}

// AddMulti 批量插入走单条 CF.INSERT：options 恒置 nil——不带 CAPACITY/
// NOCREATE，参数预分配统一经 ensureReserve 的 CF.RESERVE 通道（避免
// RESERVE 与 INSERT 双通道配置语义分裂，架构定稿）。结果 1/-1 由
// BoolSliceCmd 归一为 true/false（false=该元素插入失败，桶满/驱逐超限）。
// item 不做 marshalItem 预编码校验、直发 writer 序列化（与单条 Add 同
// 口径；回退版的整体前置校验差异见 hashImpl.AddMulti）。
func (cf *cfCmdImpl) AddMulti(ctx context.Context, items ...any) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if err := cf.ensureReserve(ctx); err != nil {
		return nil, err
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

// Reset 删除 CF.* 键并复位惰性 CF.RESERVE 闸门，后续 Add 重新按配置
// CF.RESERVE 重建。闸门复位无条件执行（defer，不区分 DEL 成败）：DEL 实际
// 执行但客户端收到网络错误时，"仅成功才复位"会让旧闸门燃尽残留 → 后续
// Add 用模块默认参数隐式重建、With* 配置静默作废；双 RESERVE 竞态的代价
// 只是 ensureReserve 吞掉的 "item exists"，无害。键不存在时 DEL 返回 0、
// 无错误，天然幂等。
func (cf *cfCmdImpl) Reset(ctx context.Context) error {
	defer cf.once.Store(new(sync.Once))
	return cf.client.Del(ctx, cf.key).Err()
}
