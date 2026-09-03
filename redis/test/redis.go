// Package test 提供 Redis 相关测试的统一辅助层：
// 统一四个环境变量（REDIS_URL / REDIS_STACK_URL / REDIS_CLUSTER_ADDRS /
// REDIS_PASSWORD）的引用与 Skip 语义，避免各测试包手写 Getenv 守卫。
package test

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/charlienet/gadget/redis"
	"github.com/stretchr/testify/assert"
)

// 环境变量语义（统一入口，所有测试包共用）：
//   - REDIS_URL           单机/哨兵 Redis 地址（URL 形式，如
//     redis://:pass@host:6379；亦支持逗号分隔的多地址种子列表）
//   - REDIS_STACK_URL     Redis Stack 地址（含 RedisBloom/ReJSON 等模块）
//   - REDIS_CLUSTER_ADDRS Redis Cluster 节点地址（逗号分隔，如
//     "192.168.2.121:7001,192.168.2.121:7002,..."）
//   - REDIS_PASSWORD      Redis 密码（可选；URL 已含密码时无需设置）

// RunOnRedis 在单机 Redis 上运行 fn。REDIS_URL 未设置时跳过（不硬失败），
// 保证无 Redis 环境的本地/CI 测试套件不受影响。
func RunOnRedis(t testing.TB, fn func(rdb redis.Client), opts ...redis.Option) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skip real-Redis test")
	}

	runOnRedis(t, fn, url, opts...)
}

// RunOnRedisStack 在 Redis Stack（含模块）上运行 fn。
// REDIS_STACK_URL 未设置时跳过。
func RunOnRedisStack(t testing.TB, fn func(rdb redis.Client), opts ...redis.Option) {
	url := os.Getenv("REDIS_STACK_URL")
	if url == "" {
		t.Skip("REDIS_STACK_URL not set; skip real-Redis test")
	}

	runOnRedis(t, fn, url, opts...)
}

// RunOnRedisCluster 在 Redis Cluster 上运行 fn。REDIS_CLUSTER_ADDRS 未设置时
// 跳过；REDIS_PASSWORD 可选（集群启用密码时设置）。客户端经 redis.WithAddrs /
// redis.WithPassword 组装，opts 为调用方附加的 Option（如 WithPrefix）。
func RunOnRedisCluster(t testing.TB, fn func(rdb redis.Client), opts ...redis.Option) {
	addrs := clusterAddrs()
	if len(addrs) == 0 {
		t.Skip("REDIS_CLUSTER_ADDRS not set; skip cluster test")
	}

	opts = append(opts, clusterOptions(addrs, os.Getenv("REDIS_PASSWORD"))...)

	run(t, fn, func() (r redis.Client, clean func(), err error) {
		rdb := redis.New(opts...)
		return rdb, func() { _ = rdb.GracefulClose(context.Background()) }, nil
	})
}

// RunOnMiniRedis 在内存版 miniredis 上运行 fn（无需任何环境变量）。
func RunOnMiniRedis(t testing.TB, fn func(rdb redis.Client)) {
	run(t, fn, func() (r redis.Client, clean func(), err error) {
		return createMiniRedis()
	})
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

// clusterAddrs 从 REDIS_CLUSTER_ADDRS 解析集群地址（逗号分隔），
// 未设置或全为空返回 nil。
func clusterAddrs() []string {
	raw := os.Getenv("REDIS_CLUSTER_ADDRS")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	addrs := make([]string, 0, len(parts))
	for _, p := range parts {
		if a := strings.TrimSpace(p); a != "" {
			addrs = append(addrs, a)
		}
	}
	return addrs
}

// clusterOptions 组装集群连接 Option 列表（密码可选，不硬编码在代码中）。
func clusterOptions(addrs []string, pwd string) []redis.Option {
	opts := []redis.Option{redis.WithAddrs(addrs)}
	if pwd != "" {
		opts = append(opts, redis.WithPassword(pwd))
	}
	return opts
}

func createMiniRedis() (r redis.Client, clean func(), err error) {
	mr, err := miniredis.Run()
	if err != nil {
		return nil, nil, err
	}

	addr := mr.Addr()
	log.Println("mini redis run at:", addr)

	rdb := redis.New(redis.WithAddr(addr))

	return rdb, func() {
		ch := make(chan struct{})

		go func() {
			_ = rdb.Close()
			mr.Close()
			close(ch)
		}()

		select {
		case <-ch:
		case <-time.After(time.Second * 5):
		}
	}, nil
}
