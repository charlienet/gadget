package redis_test

import (
	"context"
	"testing"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/redis/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompareAndSet 验证 CAS 原子比较并设置（miniredis + Lua）。
func TestCompareAndSet(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		ctx := context.Background()

		t.Run("值匹配时设置", func(t *testing.T) {
			require.NoError(t, rdb.Set(ctx, "cas:1", "old", 0).Err())
			ok, err := rdb.CompareAndSet(ctx, "cas:1", "old", "new")
			require.NoError(t, err)
			assert.True(t, ok, "值匹配应设置成功")
			val, _ := rdb.Get(ctx, "cas:1").Result()
			assert.Equal(t, "new", val)
		})

		t.Run("值不匹配不设置", func(t *testing.T) {
			require.NoError(t, rdb.Set(ctx, "cas:2", "old", 0).Err())
			ok, err := rdb.CompareAndSet(ctx, "cas:2", "wrong", "new")
			require.NoError(t, err)
			assert.False(t, ok, "值不匹配不应设置")
			val, _ := rdb.Get(ctx, "cas:2").Result()
			assert.Equal(t, "old", val, "原值应保持不变")
		})

		t.Run("nil 表示仅当不存在时设置", func(t *testing.T) {
			require.NoError(t, rdb.Del(ctx, "cas:3").Err())
			ok, err := rdb.CompareAndSet(ctx, "cas:3", nil, "v1")
			require.NoError(t, err)
			assert.True(t, ok, "key 不存在时 nil 语义应设置成功")

			// 已存在时 nil 语义失败（SETNX）
			ok, err = rdb.CompareAndSet(ctx, "cas:3", nil, "v2")
			require.NoError(t, err)
			assert.False(t, ok, "key 已存在时 nil 语义应失败")
			val, _ := rdb.Get(ctx, "cas:3").Result()
			assert.Equal(t, "v1", val, "原值应保持不变")
		})

		t.Run("key 不存在时值比较失败", func(t *testing.T) {
			require.NoError(t, rdb.Del(ctx, "cas:4").Err())
			ok, err := rdb.CompareAndSet(ctx, "cas:4", "old", "new")
			require.NoError(t, err)
			assert.False(t, ok, "key 不存在时比较应失败")
			exists, _ := rdb.Exists(ctx, "cas:4").Result()
			assert.Equal(t, int64(0), exists, "不应创建 key")
		})

		t.Run("number 兼容：按字符串比较", func(t *testing.T) {
			require.NoError(t, rdb.Set(ctx, "cas:5", "123", 0).Err())
			ok, err := rdb.CompareAndSet(ctx, "cas:5", 123, 456)
			require.NoError(t, err)
			assert.True(t, ok, "数值 123 与字符串 \"123\" 应匹配")
			val, _ := rdb.Get(ctx, "cas:5").Result()
			assert.Equal(t, "456", val)
		})
	})
}

// TestCompareAndDelete 验证 CAS 原子比较并删除（miniredis + Lua）。
func TestCompareAndDelete(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		ctx := context.Background()

		t.Run("值匹配时删除", func(t *testing.T) {
			require.NoError(t, rdb.Set(ctx, "cad:1", "old", 0).Err())
			ok, err := rdb.CompareAndDelete(ctx, "cad:1", "old")
			require.NoError(t, err)
			assert.True(t, ok, "值匹配应删除成功")
			exists, _ := rdb.Exists(ctx, "cad:1").Result()
			assert.Equal(t, int64(0), exists)
		})

		t.Run("值不匹配不删除", func(t *testing.T) {
			require.NoError(t, rdb.Set(ctx, "cad:2", "old", 0).Err())
			ok, err := rdb.CompareAndDelete(ctx, "cad:2", "wrong")
			require.NoError(t, err)
			assert.False(t, ok, "值不匹配不应删除")
			val, _ := rdb.Get(ctx, "cad:2").Result()
			assert.Equal(t, "old", val)
		})

		t.Run("nil 表示存在即删除", func(t *testing.T) {
			require.NoError(t, rdb.Set(ctx, "cad:3", "anything", 0).Err())
			ok, err := rdb.CompareAndDelete(ctx, "cad:3", nil)
			require.NoError(t, err)
			assert.True(t, ok, "nil 语义应无条件删除")
			exists, _ := rdb.Exists(ctx, "cad:3").Result()
			assert.Equal(t, int64(0), exists)
		})
	})
}
