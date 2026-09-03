package redis

import (
	"context"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	r "github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/redis/test"
	"github.com/stretchr/testify/assert"
)

// TestStoreBulkOperations 直接验证 BulkStore 实现。
// 说明：cache 包的 GetMulti 当前逐 key 走 Get（不经 BulkStore.GetMulti），
// 因此这里需要内包测试直接覆盖 GetMulti/SetMulti 的 MGet/MSet 路径。
func TestStoreBulkOperations(t *testing.T) {
	ctx := context.TODO()

	test.RunOnMiniRedis(t, func(rdb r.Client) {
		s := new(rdb)
		bulk, ok := s.(cache.BulkStore)
		assert.True(t, ok, "redis_store 应实现 BulkStore")

		// SetMulti：expire=0 → MSet 原子写入（永不过期）
		assert.NoError(t, bulk.SetMulti(ctx, map[string][]byte{
			"k1": []byte("v1"),
			"k2": []byte("v2"),
		}, 0))

		// GetMulti：全部命中
		got, err := bulk.GetMulti(ctx, "k1", "k2")
		assert.NoError(t, err)
		assert.Equal(t, map[string][]byte{"k1": []byte("v1"), "k2": []byte("v2")}, got)

		// GetMulti：部分 miss，miss 项（exist=false 语义）不出现在结果中
		got2, err := bulk.GetMulti(ctx, "k1", "k3")
		assert.NoError(t, err)
		assert.Equal(t, map[string][]byte{"k1": []byte("v1")}, got2)

		// GetMulti：空 key 列表
		got3, err := bulk.GetMulti(ctx)
		assert.NoError(t, err)
		assert.Empty(t, got3)

		// SetMulti：expire>0 → 退化为循环 Set，TTL 语义正确
		assert.NoError(t, bulk.SetMulti(ctx, map[string][]byte{"k4": []byte("v4")}, 30))
		ttl, err := rdb.TTL(ctx, "k4").Result()
		assert.NoError(t, err)
		assert.Greater(t, ttl, time.Duration(0))

		// SetMulti：空 items 无副作用
		assert.NoError(t, bulk.SetMulti(ctx, map[string][]byte{}, 60))
	})
}

// TestStoreDeletePattern 直接验证 PatternStore 实现：
//   - 空 pattern 拒绝（避免全库 SCAN）
//   - 无前缀场景删除正确
//   - 前缀场景：SCAN 返回的完整 key 剥离前缀后再 Del，避免二次加前缀
func TestStoreDeletePattern(t *testing.T) {
	ctx := context.TODO()

	test.RunOnMiniRedis(t, func(rdb r.Client) {
		s := new(rdb, WithTTLFactor(0))
		ps, ok := s.(cache.PatternStore)
		assert.True(t, ok, "redis_store 应实现 PatternStore")

		// 空 pattern 拒绝
		assert.Error(t, ps.DeletePattern(ctx, ""))

		// 无前缀场景
		assert.NoError(t, s.Put(ctx, "user:1", []byte("a"), 60))
		assert.NoError(t, s.Put(ctx, "user:2", []byte("b"), 60))
		assert.NoError(t, s.Put(ctx, "other:1", []byte("c"), 60))

		assert.NoError(t, ps.DeletePattern(ctx, "user:*"))

		_, exist, err := s.Get(ctx, "user:1")
		assert.NoError(t, err)
		assert.False(t, exist)
		_, exist, err = s.Get(ctx, "user:2")
		assert.NoError(t, err)
		assert.False(t, exist)
		v, exist, err := s.Get(ctx, "other:1")
		assert.NoError(t, err)
		assert.True(t, exist)
		assert.Equal(t, []byte("c"), v)

		// 前缀场景：Initialize 后 AddPrefix("users")，
		// 验证 SCAN 返回的完整 key 剥离前缀后 Del 不会二次加前缀
		s2 := new(rdb, WithTTLFactor(0))
		if i, ok := s2.(interface{ Initialize(cache.Options) }); ok {
			i.Initialize(cache.Options{Name: "users"})
		}

		assert.NoError(t, s2.Put(ctx, "user:1", []byte("a"), 60))
		assert.NoError(t, s2.Put(ctx, "user:2", []byte("b"), 60))

		ps2 := s2.(cache.PatternStore)
		assert.NoError(t, ps2.DeletePattern(ctx, "user:*"))

		_, exist, err = s2.Get(ctx, "user:1")
		assert.NoError(t, err)
		assert.False(t, exist)
		_, exist, err = s2.Get(ctx, "user:2")
		assert.NoError(t, err)
		assert.False(t, exist)

		// 无前缀 store 写入的 other:1 不受前缀场景删除影响
		v, exist, err = s.Get(ctx, "other:1")
		assert.NoError(t, err)
		assert.True(t, exist)
		assert.Equal(t, []byte("c"), v)
	})
}
