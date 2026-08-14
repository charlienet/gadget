package redis_test

import (
	"context"
	"testing"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCuckooFilter 验证布谷鸟过滤器（CF.* 命令族）。
// miniredis 不支持 CF.* 命令，需真实 Redis + RedisBloom 的 cuckoo 模块；
// 服务器未加载 cuckoo 模块（Capability().HasCuckoo() == false）时跳过。
func TestCuckooFilter(t *testing.T) {
	test.RunOnRedisStack(t, func(rdb redis.Client) {
		if !rdb.Capability().HasCuckoo() {
			t.Skip("服务器未加载 cuckoo 模块，跳过 CF.* 测试（需 RedisBloom）")
		}

		ctx := context.Background()
		key := "cf:test"

		// 指定容量：Add 时惰性 CF.RESERVE 预分配；先删除确保从空过滤器开始
		require.NoError(t, rdb.Del(ctx, key).Err())
		cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(10000))

		t.Run("Add 与 Exists", func(t *testing.T) {
			added, err := cf.Add(ctx, "item1")
			require.NoError(t, err)
			assert.True(t, added, "首次添加应返回新增")

			exists, err := cf.Exists(ctx, "item1")
			require.NoError(t, err)
			assert.True(t, exists)

			// 未添加的元素可能假阳性，但刚创建的空过滤器不应命中
			exists, err = cf.Exists(ctx, "never-added")
			require.NoError(t, err)
			assert.False(t, exists)
		})

		t.Run("Del 与 Info", func(t *testing.T) {
			deleted, err := cf.Del(ctx, "item1")
			require.NoError(t, err)
			assert.True(t, deleted, "已存在元素删除应成功")

			// 删除后 Exists 应返回 false（布谷鸟过滤器支持删除，无假阴性）
			exists, err := cf.Exists(ctx, "item1")
			require.NoError(t, err)
			assert.False(t, exists, "删除后元素不应再命中")

			info, err := cf.Info(ctx)
			require.NoError(t, err)
			assert.NotNil(t, info)
			assert.Greater(t, info.Size, int64(0), "过滤器应有实际大小")
		})

		t.Run("惰性创建（不指定容量）", func(t *testing.T) {
			cf2 := rdb.NewCuckooFilter("cf:test2")
			require.NoError(t, rdb.Del(ctx, "cf:test2").Err())

			added, err := cf2.Add(ctx, "x")
			require.NoError(t, err)
			assert.True(t, added, "未预分配时 CF.ADD 应惰性创建过滤器")
		})
	})
}
