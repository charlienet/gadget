package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/plugins/cache/redis"
	r "github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
	"github.com/stretchr/testify/assert"
)

func TestCache(t *testing.T) {
	ctx := context.TODO()
	key := "redistestkey"
	val := "hello go-cache"

	test.RunOnRedis(t, func(rdb r.Client) {
		t.Run("CacheGetMiss", func(t *testing.T) {
			if err := cache.New(redis.New(rdb)).Get(ctx, key, nil); err == nil {
				t.Error("expected to get no value from cache")
			}

		})

		t.Run("CacheGetHit", func(t *testing.T) {
			c := cache.New(redis.New(rdb))

			if err := c.Put(ctx, key, val, 30); err != nil {
				t.Error(err)
			}

			var s string
			if err := c.Get(ctx, key, &s); err != nil {
				t.Errorf("Expected a value, got err: %s", err)
			} else if string(s) != val {
				t.Errorf("Expected '%v', got '%v'", val, s)
			}

			assert.Equal(t, val, s)
		})

		t.Run("CacheGetExpired", func(t *testing.T) {
			// 关闭 cache 包 L1（mem）的默认防雪崩抖动（WithTTLJitter(0)）：
			// 否则 mem 的 TTL 会叠加 0~30s 随机（cache 包默认开启），
			// expireSeconds=2 实际存 2~32s，5s 后可能仍在 mem 中命中，
			// 无法验证 Redis 层 TTL 过期语义。
			c := cache.New(
				redis.New(rdb, redis.WithTTLFactor(0)),
				cache.WithTTLJitter(0),
			)
			d := 2

			if err := c.Put(ctx, key, val, d); err != nil {
				t.Error(err)
			}

			var s string
			<-time.After(5 * time.Second)
			if err := c.Get(ctx, key, &s); err == nil {
				t.Error("expected to get no value from cache")
			}
		})

	})
}

func TestMultiLevelCacheGetNotExist(t *testing.T) {
	ctx := context.TODO()

	test.RunOnMiniRedis(t, func(rdb r.Client) {
		key := "multi-level-key"

		// 创建两个双层缓存实例，共享同一个 Redis
		c1 := cache.New(
			cache.WithMemStore(),
			redis.New(rdb),
		)
		c2 := cache.New(
			cache.WithMemStore(),
			redis.New(rdb),
		)

		// 1. 两个实例都获取不存在的 key
		var s1, s2 string
		assert.ErrorIs(t, c1.Get(ctx, key, &s1), cache.ErrEntityNotExist)
		assert.ErrorIs(t, c2.Get(ctx, key, &s2), cache.ErrEntityNotExist)

		// 2. c1 删除原 key（清除可能的空值占位），然后写入新值
		c1.Delete(ctx, key)
		assert.Nil(t, c1.Put(ctx, key, "hello-from-c1", 300))

		// 3. c2 检查该 key 是否存在（本地内存 miss，回退 Redis 命中）
		var s3 string
		assert.Nil(t, c2.Get(ctx, key, &s3))
		assert.Equal(t, "hello-from-c1", s3)
	})
}

// TestPutExpireZeroNoTTL 验证 F5：expireSeconds=0 时不得叠加随机 TTL（永不过期）。
func TestPutExpireZeroNoTTL(t *testing.T) {
	ctx := context.TODO()

	test.RunOnMiniRedis(t, func(rdb r.Client) {
		// ttlFactor 非零：若 expireSeconds=0 仍叠加随机秒数即为缺陷
		c := cache.New(redis.New(rdb, redis.WithTTLFactor(30)))

		key := "redistestkey-noexpire"
		assert.NoError(t, c.Put(ctx, key, "val", 0))

		// cache 默认 Name="cache"，Redis 实际 key 为 "cache:"+key
		// go-redis 对 TTL=-1（永不过期）返回负 Duration（-1ns）
		//
		// 注意：此处断言依赖 miniredis v2.5 的 TTL 行为——miniredis 对永不过期
		// 的 key 返回负值，与真实 Redis 的 TTL=-1 语义一致；但这是 miniredis
		// 特定版本的行为（升级 miniredis 时需复核该断言是否仍然成立）。
		ttl, err := rdb.TTL(ctx, "cache:"+key).Result()
		assert.NoError(t, err)
		assert.Less(t, ttl, time.Duration(0), "expireSeconds=0 时 key 应永不过期（TTL 为负）")

		// 对照：正常 TTL 不受修复影响
		key2 := "redistestkey-expire"
		assert.NoError(t, c.Put(ctx, key2, "val", 30))
		ttl2, err := rdb.TTL(ctx, "cache:"+key2).Result()
		assert.NoError(t, err)
		assert.Greater(t, ttl2, time.Duration(0), "expireSeconds>0 时 key 应有过期时间")
	})
}

// TestTTLFactorDefaultExact 验证防雪崩随机叠加的默认开启与显式关闭：
//   - 默认 ttlFactor=30：Put(expireSeconds=100) 的 TTL 叠加 [1,29] 秒随机（101~129）；
//   - 显式 WithTTLFactor(0)：TTL 所见即所得（99~101）。
func TestTTLFactorDefaultExact(t *testing.T) {
	ctx := context.TODO()

	test.RunOnMiniRedis(t, func(rdb r.Client) {
		t.Run("默认开启叠加随机", func(t *testing.T) {
			c := cache.New(redis.New(rdb))
			key := "redistestkey-exact"

			assert.NoError(t, c.Put(ctx, key, "val", 100))

			ttl, err := rdb.TTL(ctx, "cache:"+key).Result()
			assert.NoError(t, err)
			assert.True(t, ttl >= 101*time.Second && ttl <= 129*time.Second,
				"默认 ttlFactor=30 时 TTL 应为 100+[1,29] 秒（实际 %v）", ttl)
		})

		t.Run("显式 WithTTLFactor(0) 关闭", func(t *testing.T) {
			c := cache.New(redis.New(rdb, redis.WithTTLFactor(0)))
			key := "redistestkey-exact-off"

			assert.NoError(t, c.Put(ctx, key, "val", 100))

			ttl, err := rdb.TTL(ctx, "cache:"+key).Result()
			assert.NoError(t, err)
			assert.True(t, ttl >= 99*time.Second && ttl <= 101*time.Second,
				"WithTTLFactor(0) 时 TTL 应为传入值 100s（实际 %v）", ttl)
		})
	})
}

// TestDeletePattern 验证 F1：PatternStore.DeletePattern 删除匹配 key，
// 不匹配的 key 保留（走 cache 公开 API，内部经 PatternStore 接口分派）。
func TestDeletePattern(t *testing.T) {
	ctx := context.TODO()

	test.RunOnMiniRedis(t, func(rdb r.Client) {
		c := cache.New(redis.New(rdb))

		assert.NoError(t, c.Put(ctx, "user:1", "a", 60))
		assert.NoError(t, c.Put(ctx, "user:2", "b", 60))
		assert.NoError(t, c.Put(ctx, "other:1", "c", 60))

		c.DeletePattern(ctx, "user:*")

		var s string
		assert.ErrorIs(t, c.Get(ctx, "user:1", &s), cache.ErrEntityNotExist)
		assert.ErrorIs(t, c.Get(ctx, "user:2", &s), cache.ErrEntityNotExist)

		assert.NoError(t, c.Get(ctx, "other:1", &s))
		assert.Equal(t, "c", s)
	})
}

// TestBulkSetMultiGetMulti 验证 F2：BulkStore.SetMulti（MSet/循环 Set）写入
// 的数据可正常读回，miss 项不出现在结果中。
func TestBulkSetMultiGetMulti(t *testing.T) {
	ctx := context.TODO()

	test.RunOnMiniRedis(t, func(rdb r.Client) {
		c := cache.New(redis.New(rdb))

		// expire=0：MSet 原子批量写入（永不过期）
		assert.NoError(t, c.SetMulti(ctx, map[string]any{"bulk:a": 1, "bulk:b": 2}, 0))

		// 全部命中（JSON 反序列化数值为 float64）
		got, err := c.GetMulti(ctx, "bulk:a", "bulk:b")
		assert.NoError(t, err)
		assert.Equal(t, float64(1), got["bulk:a"])
		assert.Equal(t, float64(2), got["bulk:b"])

		// 部分 miss：miss 项不出现在结果中
		got2, err := c.GetMulti(ctx, "bulk:a", "bulk:missing")
		assert.NoError(t, err)
		assert.Equal(t, float64(1), got2["bulk:a"])
		_, ok := got2["bulk:missing"]
		assert.False(t, ok)

		// expire>0：退化为循环 Set，TTL 语义正确
		assert.NoError(t, c.SetMulti(ctx, map[string]any{"bulk:c": 3}, 30))
		ttl, err := rdb.TTL(ctx, "cache:bulk:c").Result()
		assert.NoError(t, err)
		assert.Greater(t, ttl, time.Duration(0), "expire>0 时 SetMulti 应带 TTL")
	})
}
