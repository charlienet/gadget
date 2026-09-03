package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/redis/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLeakyBucket 验证漏桶限流：恒定输出速率、拒绝突发、名称隔离。
// 注：Lua 的拒绝边界（排队量 >= burst 窗口）基于毫秒整数，连续调用间
// 的毫秒漂移会使首次拒绝发生在 burst 次之后不久（而非精确第 burst+1 次），
// 因此断言采用"前 n 次全放行 + 之后必定出现拒绝"的容差形式。
func TestLeakyBucket(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		ctx := context.Background()
		require.NoError(t, rdb.FlushDB(ctx).Err(), "清空漏桶状态 key")

		t.Run("恒定速率：rate=10/s burst 内全放行超容量后拒绝", func(t *testing.T) {
			lb := rdb.NewLeakyBucket("")
			// 独立 key，避免与其他子测试共享状态
			const k = "lb-const"

			// burst 默认 = rate = 10：前 10 次必放行（diff < burst 窗口）
			for i := 0; i < 10; i++ {
				res, err := lb.Allow(ctx, k, 10)
				require.NoError(t, err)
				assert.True(t, res.Allowed, "前 10 次应放行（第 %d 次）", i+1)
				assert.Zero(t, res.RetryAfter, "放行时 RetryAfter 应为 0")
			}

			// 超容量后必定出现拒绝（容忍毫秒漂移，10 次内）
			rejected := false
			for i := 0; i < 10; i++ {
				res, err := lb.Allow(ctx, k, 10)
				require.NoError(t, err)
				if !res.Allowed {
					assert.Greater(t, res.RetryAfter, time.Duration(0), "被拒时 RetryAfter 应 > 0")
					rejected = true
					break
				}
			}
			assert.True(t, rejected, "超过容量后应出现拒绝")
		})

		t.Run("burst=1 时不允许排队：3 次内必被拒", func(t *testing.T) {
			lb := rdb.NewLeakyBucket("", redis.WithBurst(1))
			const k = "lb-burst1"

			// 毫秒精度下 burst=1 允许相邻毫秒 1-2 个请求（next 只增不减），
			// 因此第 2 次可能放行，但第 3 次（排队 ≥2 间隔）必被拒。
			res, err := lb.Allow(ctx, k, 10)
			require.NoError(t, err)
			assert.True(t, res.Allowed, "burst=1 时第 1 次应放行")

			rejected := false
			for i := 0; i < 2; i++ {
				res, err := lb.Allow(ctx, k, 10)
				require.NoError(t, err)
				if !res.Allowed {
					assert.Greater(t, res.RetryAfter, time.Duration(0), "被拒时 RetryAfter 应 > 0")
					rejected = true
					break
				}
			}
			assert.True(t, rejected, "burst=1 时 3 次内必出现拒绝")
		})

		t.Run("名称隔离：不同 name 同 key 互不影响", func(t *testing.T) {
			la := rdb.NewLeakyBucket("a", redis.WithBurst(1))
			lb := rdb.NewLeakyBucket("b", redis.WithBurst(1))
			const k = "lb-name"

			// a：3 次内出现拒绝（burst=1）
			_, err := la.Allow(ctx, k, 10)
			require.NoError(t, err)
			aRejected := false
			for i := 0; i < 2; i++ {
				res, err := la.Allow(ctx, k, 10)
				require.NoError(t, err)
				if !res.Allowed {
					aRejected = true
					break
				}
			}
			assert.True(t, aRejected, "a 应因 burst=1 被限")

			// b 的同 key 独立配额：第 1 次应放行（不受 a 影响）
			res, err := lb.Allow(ctx, k, 10)
			require.NoError(t, err)
			assert.True(t, res.Allowed, "b 的同 key 应独立放行")
		})

		t.Run("AllowN 与时间窗口", func(t *testing.T) {
			lb := rdb.NewLeakyBucket("", redis.WithBurst(2))
			const k = "lb-allown"

			// per=2s 内允许 2 个（interval=1000ms，窗口 2000ms）：
			// 前 2 次必放行，随后出现拒绝
			for i := 0; i < 2; i++ {
				res, err := lb.AllowN(ctx, k, 2, 2*time.Second)
				require.NoError(t, err)
				assert.True(t, res.Allowed, "第 %d 次应放行", i+1)
			}
			rejected := false
			for i := 0; i < 5; i++ {
				res, err := lb.AllowN(ctx, k, 2, 2*time.Second)
				require.NoError(t, err)
				if !res.Allowed {
					rejected = true
					break
				}
			}
			assert.True(t, rejected, "超过 burst 后应出现拒绝")
		})

		t.Run("速率非法值返回错误", func(t *testing.T) {
			lb := rdb.NewLeakyBucket("")
			_, err := lb.Allow(ctx, "lb-invalid", 0)
			require.Error(t, err, "rate=0 应返回错误")
		})
	})
}

// TestLeakyBucketWait 验证漏桶阻塞等待模式：等待后放行、ctx 超时返回错误。
// 注：burst=1 时 next 只领先 now 一个输出间隔，Wait 首次 Allow 的排队量
// （间隔 - 毫秒漂移）恒小于窗口会放行，因此测试先循环 Allow 直到观察到
// ≥2 次放行（next 领先 ≥2 间隔），保证后续 Wait 必先被拒。
func TestLeakyBucketWait(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		ctx := context.Background()
		require.NoError(t, rdb.FlushDB(ctx).Err())

		// ensureQueue 循环 Allow 直到至少放行 2 次，制造确定的排队。
		ensureQueue := func(lb *redis.LeakyBucket, k string, rate int) {
			released := 0
			for i := 0; i < 10 && released < 2; i++ {
				res, err := lb.Allow(ctx, k, rate)
				require.NoError(t, err)
				if res.Allowed {
					released++
				}
			}
			require.GreaterOrEqual(t, released, 2, "应至少放行 2 次以制造确定排队")
		}

		t.Run("Wait 在有排队时等待后成功放行", func(t *testing.T) {
			lb := rdb.NewLeakyBucket("", redis.WithBurst(1))
			const k = "lbw-wait"
			ensureQueue(lb, k, 5) // interval=200ms：2 次放行 → next 领先 ≥400ms

			start := time.Now()
			err := lb.Wait(ctx, k, 5)
			require.NoError(t, err)
			elapsed := time.Since(start)
			assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond,
				"Wait 应等待至少约 1 个输出间隔（200ms），实际 %v", elapsed)
		})

		t.Run("Wait ctx 超时返回 ctx.Err", func(t *testing.T) {
			lb := rdb.NewLeakyBucket("", redis.WithBurst(1))
			const k = "lbw-timeout"
			ensureQueue(lb, k, 1) // interval=1000ms：2 次放行 → next 领先 ≥2000ms

			wctx, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
			defer cancel()
			err := lb.Wait(wctx, k, 1)
			require.Error(t, err, "ctx 超时应返回错误")
			assert.Equal(t, context.DeadlineExceeded, err, "应返回 DeadlineExceeded")
		})
	})
}

// TestRateLimiterWait 验证令牌桶的阻塞等待模式。
func TestRateLimiterWait(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		ctx := context.Background()
		require.NoError(t, rdb.FlushDB(ctx).Err())

		t.Run("Wait 等待后放行", func(t *testing.T) {
			rl := rdb.NewRateLimiter("")
			const k = "rlw-wait"

			// 耗尽瞬时配额（burst=5）：连续 5 次放行
			for i := 0; i < 5; i++ {
				res, err := rl.Allow(ctx, k, 5)
				require.NoError(t, err)
				assert.True(t, res.Allowed, "第 %d 次应放行", i+1)
			}

			// 第 6 次需等待补令牌（约 200ms）后放行
			start := time.Now()
			err := rl.Wait(ctx, k, 5)
			require.NoError(t, err)
			elapsed := time.Since(start)
			assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond,
				"Wait 应等待至少约 200ms，实际 %v", elapsed)
		})

		t.Run("Wait ctx 超时返回 ctx.Err", func(t *testing.T) {
			rl := rdb.NewRateLimiter("")
			const k = "rlw-timeout"

			_, err := rl.Allow(ctx, k, 1)
			require.NoError(t, err)

			wctx, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
			defer cancel()
			err = rl.Wait(wctx, k, 1)
			require.Error(t, err)
			assert.Equal(t, context.DeadlineExceeded, err)
		})

		t.Run("WaitN 等待后放行", func(t *testing.T) {
			rl := rdb.NewRateLimiter("")
			const k = "rlw-waitn"

			_, err := rl.AllowN(ctx, k, 2, time.Second)
			require.NoError(t, err)
			// 已用掉 1 个配额，剩余 1 个：WaitN 应较快放行
			start := time.Now()
			err = rl.WaitN(ctx, k, 2, time.Second)
			require.NoError(t, err)
			assert.Less(t, time.Since(start), 2*time.Second)
		})
	})
}
