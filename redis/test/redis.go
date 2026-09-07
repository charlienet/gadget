// Package test 提供 Redis 相关测试的统一辅助层：
// 统一两个环境变量（REDIS_URL / REDIS_CLUSTER）的引用与
// Skip 语义，避免各测试包手写 Getenv 守卫。
// 内存 Redis（miniredis）入口见 redis/test/mini 子包（mini.Run）——
// 本包刻意不引入 miniredis 依赖，避免下游应用经传递依赖被拉入。
//
// 迁移说明（v0.5.0 破坏性变更）：
//   - 辅助 API：已删除 RunOnRedisStack()（改用 RunOnRedis()）、
//     RunOnMiniRedis()（改用 mini.Run()，import path 为
//     github.com/charlienet/gadget/redis/test/mini）。
//   - 环境变量：REDIS_STACK_URL 并入 REDIS_URL；REDIS_CLUSTER_ADDRS +
//     REDIS_PASSWORD 统一为 REDIS_CLUSTER（完整 URL，密码内嵌，如
//     redis://:pass@host:6379）。
package test

import (
	"os"
	"testing"

	"github.com/charlienet/gadget/redis"
	"github.com/stretchr/testify/assert"
)

// 环境变量语义（统一入口，所有测试包共用）：
//   - REDIS_URL           单机/哨兵 Redis 地址（URL 形式，如
//     redis://:pass@host:6379；亦支持逗号分隔的多地址种子列表）
//   - REDIS_CLUSTER Redis Cluster 完整 URL（如
//     redis://:pass@host1:7001,host2:7002；密码在 userinfo 中，host
//     段支持逗号分隔多地址）

// RunOnRedis 在单机 Redis 上运行 fn。REDIS_URL 未设置时跳过（不硬失败），
// 保证无 Redis 环境的本地/CI 测试套件不受影响。
func RunOnRedis(t testing.TB, fn func(rdb redis.Client), opts ...redis.Option) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skip real-Redis test")
	}

	runOnRedis(t, fn, url, opts...)
}

// RunOnRedisCluster 在 Redis Cluster 上运行 fn。REDIS_CLUSTER 为完整集群
// URL（userinfo 携带密码，host 段支持逗号分隔多地址种子），未设置时跳过。
// opts 为调用方附加的 Option（如 WithPrefix），连接配置全部从 URL 提取。
func RunOnRedisCluster(t testing.TB, fn func(rdb redis.Client), opts ...redis.Option) {
	url := os.Getenv("REDIS_CLUSTER")
	if url == "" {
		t.Skip("REDIS_CLUSTER not set; skip cluster test")
	}

	runOnRedis(t, fn, url, opts...)
}

func runOnRedis(t testing.TB, fn func(rdb redis.Client), url string, opts ...redis.Option) {
	run(t, fn, func() (r redis.Client, clean func(), err error) {
		rdb, err := newClientFromURL(url, opts...)
		if err != nil {
			return nil, nil, err
		}

		if err := rdb.Constraint(redis.Ping()); err != nil {
			return nil, nil, err
		}

		return rdb, func() { _ = rdb.Close() }, nil
	})
}

// newClientFromURL 从 URL 创建本库 Client。
// 逗号分隔的多地址（Redis Cluster 种子列表）已由本库 ParseURL 原生支持，
// 直接透传 NewWithUrl 即可（见 redis/redis.go 的 ParseURL/parseMultiAddrURL）。
func newClientFromURL(url string, opts ...redis.Option) (redis.Client, error) {
	return redis.NewWithUrl(url, opts...)
}

func run(t testing.TB, fn func(rdb redis.Client), cn func() (r redis.Client, clean func(), err error)) {
	r, clean, err := cn()

	assert.Nil(t, err, err)
	defer clean()
	fn(r)
}
