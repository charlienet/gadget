package redis_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charlienet/gadget/lock"
	redislock "github.com/charlienet/gadget/plugins/lock/redis"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyCounter 保证同一进程内多次运行 / 并发子测试生成互不相同的键。
var keyCounter uint64

// lockKey 生成一次测试作用域专用的唯一 Redis 键，并注册 t.Cleanup 删除。
// 真实 Redis 是共享且持久化的（不同于内存假服务那般每个测试天然隔离），
// 固定键会在重复运行/并发下互相残留污染；这里用唯一键 + 收尾删除复现"每测试干净状态"。
// 键带 {} hash tag，兼容 Redis Cluster。
func lockKey(t *testing.T, rdb goredis.Cmdable, base string) string {
	t.Helper()
	id := atomic.AddUint64(&keyCounter, 1)
	key := fmt.Sprintf("{gadget-lock-test:%s:%d:%d}", base, time.Now().UnixNano(), id)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = rdb.Del(ctx, key)
	})
	return key
}

// TestMain 在 REDIS_URL 未设置时向 stderr 打印显眼警告。
// 写入 stderr 保证即使非 -verbose 模式也能看到，避免真实 Redis 验证被静默跳过。
func TestMain(m *testing.M) {
	if os.Getenv("REDIS_URL") == "" {
		fmt.Fprintln(os.Stderr, "=======================================================================")
		fmt.Fprintln(os.Stderr, "[WARNING] 环境变量 REDIS_URL 未设置：")
		fmt.Fprintln(os.Stderr, "          Redis 锁语义（Lua 原子释放/续期/过期）未经真实 Redis 验证，")
		fmt.Fprintln(os.Stderr, "          以下测试将全部 SKIP。设置 REDIS_URL 后重跑以启用真实验证。")
		fmt.Fprintln(os.Stderr, "=======================================================================")
	}
	os.Exit(m.Run())
}

// newTestClient 基于真实 Redis（REDIS_URL）创建 go-redis Client。
// REDIS_URL 未设置时跳过；已设置但不可达时直接失败，避免连接问题伪装成通过。
func newTestClient(t *testing.T) goredis.Cmdable {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skipf("REDIS_URL 未设置，跳过真实 Redis 测试（锁语义未验证）")
	}

	opt, err := goredis.ParseURL(url)
	require.NoErrorf(t, err, "解析 REDIS_URL 失败: %s", url)

	rdb := goredis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoErrorf(t, rdb.Ping(ctx).Err(), "无法连接真实 Redis（%s），测试失败以避免假绿", url)

	return rdb
}

// TestBackendTryAcquire 验证 TryAcquire 的互斥与释放后重新获取语义。
func TestBackendTryAcquire(t *testing.T) {
	rdb := newTestClient(t)
	backend := redislock.New(rdb)
	ctx := context.Background()
	key := lockKey(t, rdb, "ba:1")

	t.Run("成功与互斥", func(t *testing.T) {
		ok, err := backend.TryAcquire(ctx, key, "tok-a", 30*time.Second)
		require.NoError(t, err)
		assert.True(t, ok)

		// 第二个不同 token 应失败（互斥）
		ok, err = backend.TryAcquire(ctx, key, "tok-b", 30*time.Second)
		require.NoError(t, err)
		assert.False(t, ok, "锁已被持有，第二次获取应失败")

		// 释放后可被重新获取
		require.NoError(t, backend.Release(ctx, key, "tok-a"))
		ok, err = backend.TryAcquire(ctx, key, "tok-b", 30*time.Second)
		require.NoError(t, err)
		assert.True(t, ok, "释放后应可被重新获取")
	})
}

// TestBackendRelease 防误删验证：非持有者释放不影响锁。
func TestBackendRelease(t *testing.T) {
	rdb := newTestClient(t)
	backend := redislock.New(rdb)
	ctx := context.Background()
	key := lockKey(t, rdb, "ba:2")

	_, err := backend.TryAcquire(ctx, key, "tok-a", 30*time.Second)
	require.NoError(t, err)

	// 非持有者释放：token 不匹配，不删除
	require.NoError(t, backend.Release(ctx, key, "tok-b"))
	ok, err := backend.TryAcquire(ctx, key, "tok-c", 30*time.Second)
	require.NoError(t, err)
	assert.False(t, ok, "非持有者释放后锁应仍在")

	// 真正持有者释放
	require.NoError(t, backend.Release(ctx, key, "tok-a"))
	ok, err = backend.TryAcquire(ctx, key, "tok-c", 30*time.Second)
	require.NoError(t, err)
	assert.True(t, ok)
}

// TestBackendRenew 验证续期成功与防误续。
func TestBackendRenew(t *testing.T) {
	rdb := newTestClient(t)
	backend := redislock.New(rdb)
	ctx := context.Background()
	key := lockKey(t, rdb, "ba:3")

	renewer, ok := backend.(lock.Renewer)
	require.True(t, ok, "Backend 应实现 lock.Renewer")

	_, err := backend.TryAcquire(ctx, key, "tok-a", 50*time.Millisecond)
	require.NoError(t, err)

	// 持有者续期成功
	renewed, err := renewer.Renew(ctx, key, "tok-a", time.Second)
	require.NoError(t, err)
	assert.True(t, renewed, "持有者续期应成功")

	// 非持有者续期失败
	renewed, err = renewer.Renew(ctx, key, "tok-b", time.Second)
	require.NoError(t, err)
	assert.False(t, renewed, "非持有者续期应失败")

	// 续期后锁不过期：真实等待 200ms（TTL 已被续为 1s，锁应仍在）
	time.Sleep(200 * time.Millisecond)
	ok, err = backend.TryAcquire(ctx, key, "tok-c", time.Second)
	require.NoError(t, err)
	assert.False(t, ok, "续期后锁不应过期")
}

// TestBackendExpiry 验证锁到期自动释放（真实 Redis，含惰性过期延迟）。
func TestBackendExpiry(t *testing.T) {
	rdb := newTestClient(t)
	backend := redislock.New(rdb)
	ctx := context.Background()
	key := lockKey(t, rdb, "ba:4")

	ok, err := backend.TryAcquire(ctx, key, "tok-a", 50*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, ok)

	// 真实等待 200ms（TTL 50ms 的 2 倍余量），确保锁已过期
	time.Sleep(200 * time.Millisecond)
	ok, err = backend.TryAcquire(ctx, key, "tok-b", time.Second)
	require.NoError(t, err)
	assert.True(t, ok, "锁到期后应可被重新获取")
}

// TestLockIntegration 通过 lock.Lock 驱动的完整语义测试。
func TestLockIntegration(t *testing.T) {
	rdb := newTestClient(t)
	backend := redislock.New(rdb)
	ctx := context.Background()

	t.Run("互斥", func(t *testing.T) {
		key := lockKey(t, rdb, "li:1")
		l1 := lock.New(key, lock.WithBackend(backend), lock.WithTTL(30*time.Second), lock.WithToken("t1"))
		l2 := lock.New(key, lock.WithBackend(backend), lock.WithTTL(30*time.Second), lock.WithToken("t2"))

		ok, err := l1.TryLock(ctx)
		require.NoError(t, err)
		assert.True(t, ok)

		ok, err = l2.TryLock(ctx)
		require.NoError(t, err)
		assert.False(t, ok, "锁已被持有")

		require.NoError(t, l1.Unlock(ctx))
		ok, err = l2.TryLock(ctx)
		require.NoError(t, err)
		assert.True(t, ok, "释放后应可重新获取")
	})

	t.Run("防误删", func(t *testing.T) {
		key := lockKey(t, rdb, "li:2")
		l1 := lock.New(key, lock.WithBackend(backend), lock.WithTTL(30*time.Second), lock.WithToken("t1"))
		l2 := lock.New(key, lock.WithBackend(backend), lock.WithTTL(30*time.Second), lock.WithToken("t2"))

		_, err := l1.TryLock(ctx)
		require.NoError(t, err)

		// 非持有者释放不影响
		require.NoError(t, l2.Unlock(ctx))
		ok, err := l2.TryLock(ctx)
		require.NoError(t, err)
		assert.False(t, ok)

		require.NoError(t, l1.Unlock(ctx))
		ok, err = l2.TryLock(ctx)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("过期自动释放", func(t *testing.T) {
		key := lockKey(t, rdb, "li:3")
		l1 := lock.New(key, lock.WithBackend(backend), lock.WithTTL(50*time.Millisecond), lock.WithToken("t1"))
		_, err := l1.TryLock(ctx)
		require.NoError(t, err)

		// 真实等待 200ms（TTL 50ms 的 2 倍余量），确保锁已过期
		time.Sleep(200 * time.Millisecond)

		l2 := lock.New(key, lock.WithBackend(backend), lock.WithTTL(30*time.Second), lock.WithToken("t2"))
		ok, err := l2.TryLock(ctx)
		require.NoError(t, err)
		assert.True(t, ok, "锁到期后应可重新获取")
	})

	t.Run("Renew 续期与防误续", func(t *testing.T) {
		key := lockKey(t, rdb, "li:4")
		l1 := lock.New(key, lock.WithBackend(backend), lock.WithTTL(30*time.Second), lock.WithToken("t1"))
		_, err := l1.TryLock(ctx)
		require.NoError(t, err)

		// 持有者续期成功
		renewed, err := l1.Renew(ctx, time.Second)
		require.NoError(t, err)
		assert.True(t, renewed)

		// 非持有者续期失败
		l2 := lock.New(key, lock.WithBackend(backend), lock.WithTTL(30*time.Second), lock.WithToken("t2"))
		renewed, err = l2.Renew(ctx, time.Second)
		require.NoError(t, err)
		assert.False(t, renewed)
	})

	t.Run("Token 返回值", func(t *testing.T) {
		l1 := lock.New("li:5", lock.WithBackend(backend), lock.WithToken("my-token"))
		assert.Equal(t, "my-token", l1.Token())
	})
}
