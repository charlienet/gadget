package redis_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis"
	"github.com/charlienet/gadget/redis"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCloseableClient 启动 miniredis 并返回对接它的 client 与 kill 函数：
// 服务在场时完成工厂构造（构造即连接），kill 后命令路径进入服务不可用。
func newCloseableClient(t *testing.T) (redis.Client, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.New(redis.WithAddr(mr.Addr()))
	t.Cleanup(func() { _ = rdb.GracefulClose(context.Background()) })
	return rdb, mr.Close
}

// newFailedClient 创建一个指向已关闭 miniredis 的 client（Redis 服务失效，
// 后续操作产生 dial/网络层错误）。MaxRetries=-1 关闭 go-redis 的失败重试，
// 加速测试（否则每次操作重试 3 次 + 退避，耗时约 1.7s）。
func newFailedClient(t *testing.T) redis.Client {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	addr := mr.Addr()
	mr.Close() // 关闭服务：dial 将失败（connection refused，net.OpError）

	rdb := redis.New(
		redis.WithAddr(addr),
		redis.WithRedisOptions(goredis.UniversalOptions{
			Addrs:      []string{addr},
			MaxRetries: -1, // -1 表示禁用重试（go-redis 约定），加速测试
		}),
	)
	t.Cleanup(func() { _ = rdb.GracefulClose(context.Background()) })
	return rdb
}

// TestFailoverRateLimiter 验证限流器失效兜底：FailOpen 放行、FailClosed 拒绝，
// 兜底时返回 ErrRedisUnavailable 哨兵错误（errors.Is 可感知）。
func TestFailoverRateLimiter(t *testing.T) {
	rdb := newFailedClient(t)
	ctx := t.Context()

	t.Run("FailOpen 默认：Allow 放行且返回哨兵错误", func(t *testing.T) {
		rl := rdb.NewRateLimiter("")
		res, err := rl.Allow(ctx, "k", 10)
		require.ErrorIs(t, err, redis.ErrRedisUnavailable, "FailOpen 兜底应返回哨兵错误")
		assert.True(t, res.Allowed, "FailOpen 应放行")
		assert.Equal(t, 1, res.Consumed)
		assert.Contains(t, err.Error(), "connection refused", "原始错误信息应保留")
	})

	t.Run("FailClosed：Allow 拒绝且返回哨兵错误", func(t *testing.T) {
		rl := rdb.NewRateLimiter("", redis.WithFailPolicy[*redis.RateLimiter](redis.FailClosed))
		res, err := rl.Allow(ctx, "k", 10)
		require.ErrorIs(t, err, redis.ErrRedisUnavailable)
		assert.False(t, res.Allowed, "FailClosed 应拒绝")
	})

	t.Run("FailOpen：Wait 放行但返回哨兵错误", func(t *testing.T) {
		rl := rdb.NewRateLimiter("")
		err := rl.Wait(ctx, "k", 10)
		require.ErrorIs(t, err, redis.ErrRedisUnavailable, "FailOpen Wait 放行但应返回哨兵错误")
	})

	t.Run("FailClosed：Wait 返回哨兵错误", func(t *testing.T) {
		rl := rdb.NewRateLimiter("", redis.WithFailPolicy[*redis.RateLimiter](redis.FailClosed))
		err := rl.Wait(ctx, "k", 10)
		require.ErrorIs(t, err, redis.ErrRedisUnavailable, "FailClosed Wait 应返回错误（不死循环）")
	})

	t.Run("FailOpen：AllowAtMost 放行", func(t *testing.T) {
		rl := rdb.NewRateLimiter("")
		res, err := rl.AllowAtMost(ctx, "k", 10, 5)
		require.ErrorIs(t, err, redis.ErrRedisUnavailable)
		assert.True(t, res.Allowed)
	})
}

// TestFailoverLeakyBucket 验证漏桶失效兜底。
func TestFailoverLeakyBucket(t *testing.T) {
	rdb := newFailedClient(t)
	ctx := t.Context()

	lb := rdb.NewLeakyBucket("")
	res, err := lb.Allow(ctx, "k", 10)
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	assert.True(t, res.Allowed, "FailOpen 漏桶应放行")

	lbClosed := rdb.NewLeakyBucket("", redis.WithFailPolicy[*redis.LeakyBucketConfig](redis.FailClosed))
	res, err = lbClosed.Allow(ctx, "k", 10)
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	assert.False(t, res.Allowed, "FailClosed 漏桶应拒绝")

	require.ErrorIs(t, lb.Wait(ctx, "k", 10), redis.ErrRedisUnavailable, "FailOpen Wait 放行但返回哨兵错误")
	require.ErrorIs(t, lbClosed.Wait(ctx, "k", 10), redis.ErrRedisUnavailable, "FailClosed Wait 返回哨兵错误")
}

// TestFailoverBloomFilter 验证布隆过滤器失效兜底。
// miniredis 无 bf 模块 → 走 bitmapImpl（Hash+Lua 回退）。
func TestFailoverBloomFilter(t *testing.T) {
	rdb, kill := newCloseableClient(t)
	ctx := t.Context()

	bf, err0 := rdb.NewBloomFilter(ctx, "bfk")
	require.NoError(t, err0, "服务在场时构造（bitmap 连接）应成功")
	bfClosed, err1 := rdb.NewBloomFilter(ctx, "bfk2", redis.WithFailPolicy[*redis.BloomConfig](redis.FailClosed))
	require.NoError(t, err1)
	kill() // 构造完成后服务失效——写读路径进入兜底

	added, err := bf.Add(ctx, "x")
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	assert.True(t, added, "FailOpen Add 应视为已添加")

	exists, err := bf.Exists(ctx, "x")
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	assert.True(t, exists, "FailOpen Exists 应视为存在（防穿透失效但放行业务）")

	info, err := bf.Info(ctx)
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	assert.NotNil(t, info, "FailOpen Info 应返回空结构体")

	added, err = bfClosed.Add(ctx, "x")
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	assert.False(t, added, "FailClosed Add 应返回 false")
}

// TestFailoverCuckooFilter 验证布谷鸟过滤器失效兜底（miniredis 无 cuckoo
// 模块 → 走 hashImpl 回退）。
func TestFailoverCuckooFilter(t *testing.T) {
	rdb, kill := newCloseableClient(t)
	ctx := t.Context()

	cf, err0 := rdb.NewCuckooFilter(ctx, "cfk")
	require.NoError(t, err0, "服务在场时构造（hash 类型校验）应成功")
	cfClosed, err1 := rdb.NewCuckooFilter(ctx, "cfk2", redis.WithFailPolicy[*redis.CuckooConfig](redis.FailClosed))
	require.NoError(t, err1)
	kill() // 构造完成后服务失效——写读路径进入兜底

	added, err := cf.Add(ctx, "x")
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	assert.True(t, added, "FailOpen Add 应视为成功")

	exists, err := cf.Exists(ctx, "x")
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	assert.True(t, exists, "FailOpen Exists 应视为存在")

	deleted, err := cf.Del(ctx, "x")
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	assert.True(t, deleted, "FailOpen Del 应视为删除成功")

	added, err = cfClosed.Add(ctx, "x")
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	assert.False(t, added, "FailClosed Add 应返回 false")
}

// TestFailoverCommandError 验证命令级错误不触发兜底：
// 正常 miniredis 上对 string key 执行 List 操作产生 WRONGTYPE，错误原样返回
// （不包装为 ErrRedisUnavailable）。
func TestFailoverCommandError(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.New(redis.WithAddr(mr.Addr()))
	defer func() { _ = rdb.GracefulClose(context.Background()) }()
	ctx := t.Context()

	require.NoError(t, rdb.Set(ctx, "strk", "v", 0).Err())
	_, err = rdb.LPush(ctx, "strk", "x").Result()
	require.Error(t, err, "命令级错误应原样返回")
	assert.False(t, errors.Is(err, redis.ErrRedisUnavailable), "命令级错误不应判定为兜底")
	assert.Contains(t, err.Error(), "WRONGTYPE", "应为 WRONGTYPE 命令级错误")
}
