package redis

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
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

func (r *redis_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	data, err := r.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.NotFound) {
			return []byte{}, false, nil
		}

		return []byte{}, false, err
	}

	return data, true, nil
}

func (r *redis_store) Put(ctx context.Context, key string, data []byte, expireSeconds int) error {
	return r.rdb.Set(ctx, key, data, r.ttl(expireSeconds)).Err()
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
	return r.rdb.Del(ctx, key...).Err()
}

// GetMulti 批量读取（MGET）。miss 的 key 不出现在返回的 map 中，
// 与单值 Get 的 exist=false 语义一致。key 经前缀 hook 自动加业务前缀。
func (r *redis_store) GetMulti(ctx context.Context, keys ...string) (map[string][]byte, error) {
	if len(keys) == 0 {
		return map[string][]byte{}, nil
	}

	vals, err := r.rdb.MGet(ctx, keys...).Result()
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

	return r.rdb.MSet(ctx, pairs...).Err()
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
		keys, next, err := r.rdb.Scan(ctx, cursor, fullPattern, scanBatchSize).Result()
		if err != nil {
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

			if err := r.rdb.Del(ctx, toDelete...).Err(); err != nil {
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
