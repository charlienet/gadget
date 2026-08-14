package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAllowAtMost 验证 AllowAtMost 的"尽力而为"语义：
// 配额充足全部消耗、配额不足部分放行（Consumed=剩余量）、无配额拒绝。
func TestAllowAtMost(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		ctx := context.Background()
		require.NoError(t, rdb.FlushDB(ctx).Err())

		t.Run("配额充足：cost 全部消耗", func(t *testing.T) {
			rl := rdb.NewRateLimiter("")
			const k = "am-full"

			res, err := rl.AllowAtMost(ctx, k, 5, 1)
			require.NoError(t, err)
			assert.True(t, res.Allowed)
			assert.Equal(t, 1, res.Consumed, "配额充足应消耗全部 cost")
		})

		t.Run("配额不足：部分放行消耗剩余量", func(t *testing.T) {
			rl := rdb.NewRateLimiter("")
			const k = "am-partial"

			// burst=5：3 次 Allow 后剩余 2（GCRA remaining=2）
			for i := 0; i < 3; i++ {
				res, err := rl.Allow(ctx, k, 5)
				require.NoError(t, err)
				assert.True(t, res.Allowed)
			}

			// cost=10 超出剩余 2：部分放行 Consumed=2、Remaining=0
			res, err := rl.AllowAtMost(ctx, k, 5, 10)
			require.NoError(t, err)
			assert.True(t, res.Allowed, "剩余配额 >0 时应部分放行")
			assert.Equal(t, 2, res.Consumed, "应消耗剩余配额 2")
			assert.Equal(t, 0, res.Remaining, "部分放行后剩余应为 0")
		})

		t.Run("无剩余配额：拒绝", func(t *testing.T) {
			rl := rdb.NewRateLimiter("")
			const k = "am-empty"

			// 耗尽 burst=5
			for i := 0; i < 5; i++ {
				_, err := rl.Allow(ctx, k, 5)
				require.NoError(t, err)
			}

			res, err := rl.AllowAtMost(ctx, k, 5, 1)
			require.NoError(t, err)
			assert.False(t, res.Allowed, "无剩余配额应拒绝")
			assert.Equal(t, 0, res.Consumed)
			assert.Greater(t, res.RetryAfter, time.Duration(0), "拒绝时 RetryAfter 应 > 0")
		})

		t.Run("AllowAtMostN 与非法参数", func(t *testing.T) {
			rl := rdb.NewRateLimiter("")
			const k = "am-n"

			res, err := rl.AllowAtMostN(ctx, k, 2, time.Second, 1)
			require.NoError(t, err)
			assert.True(t, res.Allowed)
			assert.Equal(t, 1, res.Consumed)

			_, err = rl.AllowAtMost(ctx, k, 0, 1)
			require.Error(t, err, "rate=0 应返回错误")
			_, err = rl.AllowAtMost(ctx, k, 5, 0)
			require.Error(t, err, "cost=0 应返回错误")
		})

		t.Run("名称隔离下工作正常", func(t *testing.T) {
			la := rdb.NewRateLimiter("a")
			lb := rdb.NewRateLimiter("b")
			const k = "am-name"

			// a 耗尽
			for i := 0; i < 5; i++ {
				_, err := la.Allow(ctx, k, 5)
				require.NoError(t, err)
			}
			res, err := la.AllowAtMost(ctx, k, 5, 1)
			require.NoError(t, err)
			assert.False(t, res.Allowed, "a 已耗尽应拒绝")

			// b 的同 key 独立配额：可正常消耗
			res, err = lb.AllowAtMost(ctx, k, 5, 3)
			require.NoError(t, err)
			assert.True(t, res.Allowed, "b 的同 key 应独立放行")
			assert.Equal(t, 3, res.Consumed)
		})
	})
}

// TestReset 验证 Reset 重置限流状态：耗尽后重置可再次放行，状态 key 被删除。
func TestReset(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.New(redis.WithAddr(mr.Addr()))
	defer func() { _ = rdb.GracefulClose(context.Background()) }()
	ctx := context.Background()

	rl := rdb.NewRateLimiter("")
	const k = "reset"

	// 耗尽配额
	for i := 0; i < 5; i++ {
		res, err := rl.Allow(ctx, k, 5)
		require.NoError(t, err)
		assert.True(t, res.Allowed)
	}
	// 耗尽后拒绝，状态 key 存在
	res, err := rl.Allow(ctx, k, 5)
	require.NoError(t, err)
	assert.False(t, res.Allowed, "耗尽后应拒绝")
	assert.True(t, mr.Exists("rate:"+k), "限流状态 key 应存在")

	// Reset：删除状态 key，配额恢复满额
	require.NoError(t, rl.Reset(ctx, k))
	assert.False(t, mr.Exists("rate:"+k), "Reset 后状态 key 应被删除")

	res, err = rl.Allow(ctx, k, 5)
	require.NoError(t, err)
	assert.True(t, res.Allowed, "Reset 后应可再次放行（配额恢复满额）")
}
