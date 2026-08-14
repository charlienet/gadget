package redis_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/charlienet/gadget/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAutoReconnect 验证 go-redis 连接池的自动重连（惰性重建）：
// Redis 宕机后恢复，client 无需任何额外配置即可继续工作。
//
// 原理：go-redis 连接池在请求时获取连接、失败时重新 dial——宕机期间
// 操作报连接错误（可被 isUnavailable 判定、触发兜底），恢复后新请求
// 自动重新建立连接（宕机前建立的坏连接被丢弃），无需显式重连代码。
//
// 实现：miniredis 固定端口模拟宕机→恢复：
//  1. mr1 绑定指定端口，client 正常读写
//  2. mr1.Close() 模拟宕机，操作失败
//  3. mr2 绑定同一端口模拟恢复，再次读写成功（自动重连验证）
func TestAutoReconnect(t *testing.T) {
	ctx := context.Background()

	// 选择一个可用端口（探测避免冲突；被占则跳过）
	port := 16379
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("端口 %d 被占用，跳过自动重连测试", port)
	}
	_ = ln.Close() // 释放端口给 miniredis

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// 1. 启动 miniredis（固定端口），client 正常读写
	mr1 := miniredis.NewMiniRedis()
	require.NoError(t, mr1.StartAddr(addr), "miniredis 应能绑定端口")
	defer mr1.Close()

	rdb := redis.New(redis.WithAddr(addr))
	defer func() { _ = rdb.GracefulClose(context.Background()) }()

	require.NoError(t, rdb.Set(ctx, "k", "v1", 0).Err(), "宕机前写入应正常")
	val, err := rdb.Get(ctx, "k").Result()
	require.NoError(t, err)
	assert.Equal(t, "v1", val)

	// 2. 模拟宕机：关闭 mr1，操作失败（连接错误）
	mr1.Close()
	_, err = rdb.Set(ctx, "k2", "v2", 0).Result()
	require.Error(t, err, "宕机期间操作应失败")

	// 3. 模拟恢复：同端口启动 mr2，client 自动重连后读写成功
	mr2 := miniredis.NewMiniRedis()
	require.NoError(t, mr2.StartAddr(addr), "恢复后应能绑定同一端口")
	defer mr2.Close()

	// 等待连接池内的坏连接被淘汰（go-redis 惰性重建，无需等待，
	// 但重试/淘汰有最小间隔；循环重试保证稳定）
	require.Eventually(t, func() bool {
		err := rdb.Set(ctx, "k3", "v3", 0).Err()
		return err == nil
	}, 5*time.Second, 50*time.Millisecond, "恢复后应自动重连成功")

	val, err = rdb.Get(ctx, "k3").Result()
	require.NoError(t, err)
	assert.Equal(t, "v3", val, "恢复后读写应正常（自动重连）")
}
