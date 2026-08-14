package redis_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/charlienet/gadget/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBreakerTriggerAndFailFast 验证熔断触发与快速失败：
// 服务宕机后连续失败达阈值 → Open；Open 后操作快速失败（不实际连接），
// 单次耗时远小于首次连接失败耗时。
func TestBreakerTriggerAndFailFast(t *testing.T) {
	ctx := context.Background()

	// 固定端口 miniredis（可复用端口做恢复验证，此处仅测触发）
	port := 16380
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("端口 %d 被占用，跳过", port)
	}
	_ = ln.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	mr := miniredis.NewMiniRedis()
	require.NoError(t, mr.StartAddr(addr))
	defer mr.Close()

	rdb := redis.New(redis.WithAddr(addr))
	defer func() { _ = rdb.GracefulClose(context.Background()) }()

	// 正常连接
	require.NoError(t, rdb.Ping(ctx).Err())

	// 宕机：连续失败达阈值（默认 3）后进入 Open
	mr.Close()

	var firstDuration time.Duration
	openReached := false
	for i := 0; i < 10; i++ {
		start := time.Now()
		err := rdb.Ping(ctx).Err()
		elapsed := time.Since(start)
		if err == nil {
			continue // 连接池可能还有健康连接（miniredis 关闭后不应有，防御性跳过）
		}
		if i == 0 {
			firstDuration = elapsed // 首次失败：需实际连接尝试
		}
		// 触发 Open 后（第 4 次起）应快速失败：耗时远小于首次
		if i >= 3 {
			assert.Less(t, elapsed, 100*time.Millisecond,
				"Open 状态应快速失败（第 %d 次耗时 %v，首次 %v）", i, elapsed, firstDuration)
			openReached = true
		}
	}
	require.True(t, openReached, "连续失败后应进入 Open 快速失败")
}

// TestBreakerRecover 验证熔断恢复：Open 冷却后半开放行探测，
// 服务恢复后探测成功 → Closed，正常请求放行。
func TestBreakerRecover(t *testing.T) {
	ctx := context.Background()

	port := 16381
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("端口 %d 被占用，跳过", port)
	}
	_ = ln.Close()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// 短冷却（50ms）加速测试
	rdb := redis.New(
		redis.WithAddr(addr),
		redis.WithBreakerThreshold(2),
		redis.WithBreakerCooldown(50*time.Millisecond),
	)
	defer func() { _ = rdb.GracefulClose(context.Background()) }()

	// 启动 mr1 正常连接
	mr1 := miniredis.NewMiniRedis()
	require.NoError(t, mr1.StartAddr(addr))
	require.NoError(t, rdb.Set(ctx, "k", "v1", 0).Err())

	// 宕机：连续失败触发 Open
	mr1.Close()
	require.Error(t, rdb.Set(ctx, "k2", "v2", 0).Err())
	require.Error(t, rdb.Set(ctx, "k2", "v2", 0).Err()) // 达阈值（2）→ Open

	// 冷却 50ms 后进入半开；此时同端口恢复服务
	mr2 := miniredis.NewMiniRedis()
	require.NoError(t, mr2.StartAddr(addr))
	defer mr2.Close()

	// 半开探测放行 → 服务已恢复 → 成功 → Closed → 正常请求
	require.Eventually(t, func() bool {
		return rdb.Set(ctx, "k3", "v3", 0).Err() == nil
	}, 3*time.Second, 20*time.Millisecond, "服务恢复后熔断应自动闭合，请求成功")

	val, err := rdb.Get(ctx, "k3").Result()
	require.NoError(t, err)
	assert.Equal(t, "v3", val)
}

// TestBreakerCommandError 验证命令级错误不触发熔断：
// WRONGTYPE 反复出现不进入 Open（后续正常命令仍放行）。
func TestBreakerCommandError(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.New(redis.WithAddr(mr.Addr()))
	defer func() { _ = rdb.GracefulClose(context.Background()) }()
	ctx := context.Background()

	require.NoError(t, rdb.Set(ctx, "strk", "v", 0).Err())

	// 反复命令级错误（WRONGTYPE）
	for i := 0; i < 10; i++ {
		_, err := rdb.LPush(ctx, "strk", "x").Result()
		require.Error(t, err)
		assert.False(t, errors.Is(err, redis.ErrRedisUnavailable))
	}

	// 熔断未被触发：正常命令仍放行
	require.NoError(t, rdb.Ping(ctx).Err(), "命令级错误不应触发熔断")
	val, err := rdb.Get(ctx, "strk").Result()
	require.NoError(t, err)
	assert.Equal(t, "v", val)
}

// TestBreakerFailoverLink 验证熔断与兜底联动：
// 熔断 Open 快速失败的错误（lastErr 为连接类错误）能被扩展层 isUnavailable
// 识别并走 FailPolicy 兜底（errors.Is(ErrRedisUnavailable) 命中）。
func TestBreakerFailoverLink(t *testing.T) {
	ctx := context.Background()

	port := 16382
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("端口 %d 被占用，跳过", port)
	}
	_ = ln.Close()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	rdb := redis.New(
		redis.WithAddr(addr),
		redis.WithBreakerThreshold(1), // 1 次失败即 Open，快速进入熔断
		redis.WithBreakerCooldown(50*time.Millisecond),
	)
	defer func() { _ = rdb.GracefulClose(context.Background()) }()

	// 未监听端口：首次操作失败（dial refused）→ 达阈值 Open
	require.Error(t, rdb.Ping(ctx).Err())

	// 熔断 Open：扩展操作快速失败并走兜底
	rl := rdb.NewRateLimiter("")
	res, err := rl.Allow(ctx, "k", 10)
	require.ErrorIs(t, err, redis.ErrRedisUnavailable,
		"熔断拦截的错误应被识别并走兜底（哨兵错误）")
	assert.True(t, res.Allowed, "FailOpen 限流应放行")

	lock := rdb.NewLock("lk") // 默认 FailClosed
	ok, err := lock.TryLock(ctx)
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	assert.False(t, ok, "FailClosed 锁应拒绝")
}
