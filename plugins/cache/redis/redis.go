package redis

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/retry"
)

const (
	// 默认 ttlFactor=30：防雪崩随机偏移默认开启——Put 的 expireSeconds>0 时
	// 叠加 [1, 29] 秒随机，使同批 key 过期时间分散（F5 已保证 expireSeconds<=0
	// 不叠加，永不过期语义不受影响）。需所见即所得时显式 WithTTLFactor(0)
	// 或 (1) 关闭（0/1 均不叠加）。
	defaultRedisTTLFactor = 30
	scanBatchSize         = 100
)

// 编译期断言：redis_store 必须满足 Store 以及可选的 PatternStore/BulkStore 契约。
var (
	_ cache.Store        = &redis_store{}
	_ cache.PatternStore = &redis_store{}
	_ cache.BulkStore    = &redis_store{}
)

type redis_store struct {
	rdb       redis.Client
	ttlFactor int
	retryOn   bool
	retryOpts []retry.Option
}

func new(rdb redis.Client, opts ...option) cache.Store {
	s := &redis_store{rdb: rdb, ttlFactor: defaultRedisTTLFactor}
	for _, o := range opts {
		o(s)
	}

	return s
}

func (r *redis_store) Initialize(opt cache.Options) {
	if len(opt.Name) > 0 {
		r.rdb = r.rdb.AddPrefix(opt.Name)
	}
}

// do 是所有单次 Redis 命令的操作级重试汇聚点（opt-in）。
// retryOn 关闭时直接执行 fn（行为与未接入重试完全一致）；开启时套用插件默认
// 策略（3 次尝试 + EqualJitter 指数退避 + 仅 IsUnavailable 类错误可重试），
// 并在默认之后追加用户自定义 opts 以允许覆盖。
// 默认退避在每次调用时新建（EqualJitter(Exponential(...)) 返回全新实例），
// 因此可被多个 goroutine 并发安全使用；用户若传入共享 Backoff 实例则不保证并发安全。
//
// 与熔断的分层关系：本重试位于熔断器（gadget/redis breakerHook）之外/之上层，
// L3 退避累计时长可跨过 breaker 的冷却窗口，给其在重试间隙闭合的机会；
// breaker 处于 Open 态时快速失败的错误属 IsUnavailable 类，因此可被本重试重试。
func (r *redis_store) do(ctx context.Context, fn func(ctx context.Context) error) error {
	if !r.retryOn {
		return fn(ctx)
	}

	opts := make([]retry.Option, 0, 3+len(r.retryOpts))
	opts = append(opts,
		retry.WithMaxAttempts(3),
		retry.WithBackoff(retry.EqualJitter(retry.Exponential(50*time.Millisecond, 2, time.Second))),
		retry.WithRetryable(redis.IsUnavailable),
	)
	opts = append(opts, r.retryOpts...)

	return retry.Do(ctx, fn, opts...)
}

func (r *redis_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var data []byte
	err := r.do(ctx, func(ctx context.Context) error {
		var e error
		data, e = r.rdb.Get(ctx, key).Bytes()
		return e
	})
	if err != nil {
		if errors.Is(err, redis.NotFound) {
			return []byte{}, false, nil
		}

		return []byte{}, false, err
	}

	return data, true, nil
}

func (r *redis_store) Put(ctx context.Context, key string, data []byte, expireSeconds int) error {
	ttl := r.ttl(expireSeconds)

	return r.do(ctx, func(ctx context.Context) error {
		return r.rdb.Set(ctx, key, data, ttl).Err()
	})
}

// ttl 计算写入 TTL：
//   - expireSeconds <= 0 表示永不过期，直接 Set 且不叠加随机 factor；
//   - ttlFactor > 1 时叠加 [1, ttlFactor-1] 秒随机偏移（防缓存雪崩，
//     同批 key 过期时间分散；默认 30 开启，叠加 [1,29] 秒）；
//   - ttlFactor 为 0 或 1 时均不叠加（显式关闭，TTL 所见即所得）。
func (r *redis_store) ttl(expireSeconds int) time.Duration {
	if expireSeconds <= 0 {
		return 0
	}

	factor := 0
	if r.ttlFactor > 1 {
		factor = rand.Intn(r.ttlFactor-1) + 1
	}

	return time.Second * time.Duration(expireSeconds+factor)
}

func (r *redis_store) Delete(ctx context.Context, key ...string) error {
	return r.do(ctx, func(ctx context.Context) error {
		return r.rdb.Del(ctx, key...).Err()
	})
}

// GetMulti 批量读取（MGET）。miss 的 key 不出现在返回的 map 中，
// 与单值 Get 的 exist=false 语义一致。key 经前缀 hook 自动加业务前缀。
func (r *redis_store) GetMulti(ctx context.Context, keys ...string) (map[string][]byte, error) {
	if len(keys) == 0 {
		return map[string][]byte{}, nil
	}

	var vals []interface{}
	err := r.do(ctx, func(ctx context.Context) error {
		var e error
		vals, e = r.rdb.MGet(ctx, keys...).Result()
		return e
	})
	if err != nil {
		return nil, err
	}

	result := make(map[string][]byte, len(keys))
	for i, v := range vals {
		if v == nil {
			continue // miss
		}

		s, ok := v.(string)
		if !ok {
			continue
		}
		result[keys[i]] = []byte(s)
	}

	return result, nil
}

// SetMulti 批量写入。MSet 不支持逐 key TTL，因此：
//   - expireSecond > 0：退化为循环 Set（每个 key 应用与单值 Put 一致的随机 TTL），
//     保证 TTL 语义正确（非原子，best-effort）；
//   - expireSecond <= 0（永不过期）：MSet 原子批量写入。
func (r *redis_store) SetMulti(ctx context.Context, items map[string][]byte, expireSecond int) error {
	if len(items) == 0 {
		return nil
	}

	if expireSecond > 0 {
		for key, val := range items {
			if err := r.Put(ctx, key, val, expireSecond); err != nil {
				return err
			}
		}
		return nil
	}

	pairs := make([]interface{}, 0, len(items)*2)
	for key, val := range items {
		pairs = append(pairs, key, val)
	}

	return r.do(ctx, func(ctx context.Context) error {
		return r.rdb.MSet(ctx, pairs...).Err()
	})
}

// DeletePattern 删除所有匹配 glob pattern 的 key（pattern 经业务前缀拼接，
// 例如 "user:*" → "cache:user:*"）。pattern 为空时返回错误，拒绝全库 SCAN。
//
// 前缀处理说明：SCAN 命令不在前缀 hook 的改写范围内（见 redis/hook.go 的
// 排除列表），因此 pattern 需手动拼接完整前缀（ComposeKey）；SCAN 返回的
// key 是带前缀的完整 key，而 Del 经前缀 hook 会自动加前缀，故先剥离完整前缀
// 恢复为业务 key 再批量删除，避免二次加前缀导致删不到。
func (r *redis_store) DeletePattern(ctx context.Context, pattern string) error {
	if pattern == "" {
		return errors.New("redis: DeletePattern: empty pattern is not allowed")
	}

	fullPattern := r.rdb.ComposeKey(pattern)
	prefix := r.rdb.Prefix()
	separator := r.rdb.Separator()

	var cursor uint64
	for {
		var keys []string
		var next uint64
		// 只包裹 Scan 单点：重试期间 cursor 尚未推进，语义安全。
		if err := r.do(ctx, func(ctx context.Context) error {
			var e error
			keys, next, e = r.rdb.Scan(ctx, cursor, fullPattern, scanBatchSize).Result()
			return e
		}); err != nil {
			return err
		}

		if len(keys) > 0 {
			toDelete := make([]string, 0, len(keys))
			for _, k := range keys {
				if prefix != "" {
					k = strings.TrimPrefix(k, prefix+separator)
				}
				toDelete = append(toDelete, k)
			}

			// 只包裹批量 Del 单点：不包裹整个游标循环（保留部分成功语义）。
			if err := r.do(ctx, func(ctx context.Context) error {
				return r.rdb.Del(ctx, toDelete...).Err()
			}); err != nil {
				return err
			}
		}

		cursor = next
		if cursor == 0 {
			break
		}
	}

	return nil
}

func (*redis_store) Name() string { return "Redis" }

func (*redis_store) IsRemote() bool { return true }
