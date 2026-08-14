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

// TestLock 验证分布式锁的获取/互斥/防误删/释放语义（miniredis 支持 SETNX 与 Lua）。
func TestLock(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		ctx := context.Background()

		t.Run("TryLock 成功与互斥", func(t *testing.T) {
			lock1 := rdb.NewLock("lock:1")
			lock2 := rdb.NewLock("lock:1")

			ok, err := lock1.TryLock(ctx)
			require.NoError(t, err)
			assert.True(t, ok)

			// 第二个实例获取同一把锁应失败（互斥）
			ok, err = lock2.TryLock(ctx)
			require.NoError(t, err)
			assert.False(t, ok, "锁已被持有，第二次获取应失败")

			// 释放后 lock2 可获取
			require.NoError(t, lock1.Unlock(ctx))
			ok, err = lock2.TryLock(ctx)
			require.NoError(t, err)
			assert.True(t, ok, "释放后应可被重新获取")
		})

		t.Run("Unlock 防误删他人锁", func(t *testing.T) {
			lock1 := rdb.NewLock("lock:2")
			lock2 := rdb.NewLock("lock:2") // 不同 token

			_, err := lock1.TryLock(ctx)
			require.NoError(t, err)

			// 非持有者 Unlock：Lua 校验 token 不匹配，不删除
			require.NoError(t, lock2.Unlock(ctx))
			ok, err := lock2.TryLock(ctx)
			require.NoError(t, err)
			assert.False(t, ok, "非持有者释放后锁应仍在")

			// 真正持有者释放后 lock2 可获取
			require.NoError(t, lock1.Unlock(ctx))
			ok, err = lock2.TryLock(ctx)
			require.NoError(t, err)
			assert.True(t, ok)
		})

		t.Run("Lock 阻塞获取", func(t *testing.T) {
			lock1 := rdb.NewLock("lock:3", redis.WithTTL(500*time.Millisecond))
			lock2 := rdb.NewLock("lock:3", redis.WithTTL(500*time.Millisecond))

			_, err := lock1.TryLock(ctx)
			require.NoError(t, err)

			// 异步释放后 lock2 的阻塞获取应成功
			go func() {
				time.Sleep(50 * time.Millisecond)
				_ = lock1.Unlock(context.Background())
			}()
			require.NoError(t, lock2.Lock(ctx), "阻塞获取应在锁释放后成功")
		})
	})
}

// TestLockExpiryAndRenew 验证锁到期自动释放与续期（miniredis 虚拟时间）。
func TestLockExpiryAndRenew(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.New(redis.WithAddr(mr.Addr()))
	defer func() { _ = rdb.GracefulClose(context.Background()) }()

	ctx := context.Background()

	t.Run("锁到期自动释放", func(t *testing.T) {
		lock := rdb.NewLock("lock:exp", redis.WithTTL(50*time.Millisecond))
		ok, err := lock.TryLock(ctx)
		require.NoError(t, err)
		assert.True(t, ok)

		// 推进 miniredis 虚拟时间使锁过期，其他实例应可获取
		mr.FastForward(100 * time.Millisecond)
		lock2 := rdb.NewLock("lock:exp")
		ok, err = lock2.TryLock(ctx)
		require.NoError(t, err)
		assert.True(t, ok, "锁到期后应可被重新获取")
	})

	t.Run("Renew 续期与防误续", func(t *testing.T) {
		lock := rdb.NewLock("lock:renew")
		_, err := lock.TryLock(ctx)
		require.NoError(t, err)

		// 持有者续期成功
		renewed, err := lock.Renew(ctx, time.Second)
		require.NoError(t, err)
		assert.True(t, renewed, "持有者续期应成功")

		// 非持有者（不同 token）续期失败
		lock2 := rdb.NewLock("lock:renew")
		renewed, err = lock2.Renew(ctx, time.Second)
		require.NoError(t, err)
		assert.False(t, renewed, "非持有者续期应失败")
	})

	t.Run("Renew 非正 ttl 返回错误", func(t *testing.T) {
		lock := rdb.NewLock("lock:badttl")
		_, err := lock.TryLock(ctx)
		require.NoError(t, err)

		// ttl<=0 会触发 PEXPIRE 立即过期释放锁（临界区并发风险），必须拒绝
		_, err = lock.Renew(ctx, 0)
		require.Error(t, err, "ttl=0 应返回参数错误")
		_, err = lock.Renew(ctx, -time.Second)
		require.Error(t, err, "负 ttl 应返回参数错误")
	})

	t.Run("指定 token 可检测重入", func(t *testing.T) {
		l1 := rdb.NewLock("lock:token", redis.WithToken("tok-1"))
		l2 := rdb.NewLock("lock:token", redis.WithToken("tok-1"))

		ok, err := l1.TryLock(ctx)
		require.NoError(t, err)
		assert.True(t, ok)

		// 同 token 的另一个实例获取同一把锁失败（检测重入）
		ok, err = l2.TryLock(ctx)
		require.NoError(t, err)
		assert.False(t, ok, "同 token 重复获取应失败")
		assert.Equal(t, "tok-1", l1.Token())
	})
}
