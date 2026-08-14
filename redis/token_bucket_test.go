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

// TestTokenBucketGCRA 验证自研 GCRA 令牌桶语义（替换 redis_rate 依赖后）：
// 突发放行、超限拒绝、Remaining 递减、过期清理、RetryAfter 精度。
func TestTokenBucketGCRA(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		ctx := context.Background()
		require.NoError(t, rdb.FlushDB(ctx).Err(), "清空限流 key")

		t.Run("rate=10/s：前 10 次放行后出现拒绝", func(t *testing.T) {
			rl := rdb.NewRateLimiter("")
			const k = "tb-10"

			// burst = rate = 10：GCRA 浮点精度下前 10 次必放行
			for i := 0; i < 10; i++ {
				res, err := rl.Allow(ctx, k, 10)
				require.NoError(t, err)
				assert.True(t, res.Allowed, "前 10 次应放行（第 %d 次）", i+1)
			}

			// 第 11 次必拒（浮点 tat 精确，无毫秒截断问题），RetryAfter > 0
			res, err := rl.Allow(ctx, k, 10)
			require.NoError(t, err)
			assert.False(t, res.Allowed, "第 11 次应被拒")
			assert.Greater(t, res.RetryAfter, time.Duration(0), "被拒时 RetryAfter 应 > 0")
		})

		t.Run("AllowN：n=5/秒 第 6 次被拒", func(t *testing.T) {
			rl := rdb.NewRateLimiter("")
			const k = "tb-allown"

			for i := 0; i < 5; i++ {
				res, err := rl.AllowN(ctx, k, 5, time.Second)
				require.NoError(t, err)
				assert.True(t, res.Allowed, "前 5 次应放行（第 %d 次）", i+1)
			}
			res, err := rl.AllowN(ctx, k, 5, time.Second)
			require.NoError(t, err)
			assert.False(t, res.Allowed, "第 6 次应被拒")
		})

		t.Run("Remaining 递减：n=2/秒 从 1 递减到 0 后拒绝", func(t *testing.T) {
			rl := rdb.NewRateLimiter("")
			const k = "tb-remaining"

			// burst=2：第 1 次 remaining=1（还能立即放 1 个），第 2 次=0，第 3 次拒
			res1, err := rl.AllowN(ctx, k, 2, time.Second)
			require.NoError(t, err)
			assert.True(t, res1.Allowed)
			assert.Equal(t, 1, res1.Remaining, "第 1 次后剩余 1")

			res2, err := rl.AllowN(ctx, k, 2, time.Second)
			require.NoError(t, err)
			assert.True(t, res2.Allowed)
			assert.Equal(t, 0, res2.Remaining, "第 2 次后剩余 0")

			res3, err := rl.AllowN(ctx, k, 2, time.Second)
			require.NoError(t, err)
			assert.False(t, res3.Allowed, "第 3 次应被拒")
		})

		t.Run("非法参数返回错误", func(t *testing.T) {
			rl := rdb.NewRateLimiter("")
			_, err := rl.Allow(ctx, "tb-invalid", 0)
			require.Error(t, err, "rate=0 应返回错误")
			_, err = rl.AllowN(ctx, "tb-invalid", 0, time.Second)
			require.Error(t, err, "n=0 应返回错误")
		})
	})
}

// TestTokenBucketExpiry 验证限流状态 key 带 EX 过期（防 key 堆积）。
func TestTokenBucketExpiry(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.New(redis.WithAddr(mr.Addr()))
	defer func() { _ = rdb.GracefulClose(context.Background()) }()
	ctx := context.Background()

	rl := rdb.NewRateLimiter("")
	const k = "tb-exp"
	res, err := rl.Allow(ctx, k, 1)
	require.NoError(t, err)
	assert.True(t, res.Allowed)

	// 状态 key 存在（"rate:" + key），带 EX（第 1 次后 reset_after≈1s）
	assert.True(t, mr.Exists("rate:"+k), "限流状态 key 应存在")
	assert.True(t, mr.TTL("rate:"+k) > 0, "状态 key 应带过期时间")

	// 推进虚拟时间使 key 过期
	mr.FastForward(3 * time.Second)
	assert.False(t, mr.Exists("rate:"+k), "过期后状态 key 应被清理")
}
