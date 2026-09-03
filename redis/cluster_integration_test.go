package redis_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/redis/test"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clusterAddrs 从环境变量 REDIS_CLUSTER_ADDRS 读取集群地址（逗号分隔，
// 如 "192.168.2.121:7001,192.168.2.121:7002,..."），未设置或为空返回 nil。
// 仅保留薄包装供 LoadFunction 子测试构建独立 goredis ClusterClient 使用，
// 连接建立与 Skip 守卫统一走 test.RunOnRedisCluster。
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

// clusterPassword 从环境变量 REDIS_PASSWORD 读取集群密码，未设置返回空串
// （不设密码）。密码不硬编码在代码中。
func clusterPassword() string {
	return os.Getenv("REDIS_PASSWORD")
}

// TestClusterIntegration 真实 Redis Cluster 集成验证。
// 覆盖：集群连接与模式判断、前缀在集群重定向下只加一次、LoadFunction
// 全主节点加载、Capability 探测、AddPrefix 派生子池在集群下的读写。
// 需要环境变量 REDIS_CLUSTER_ADDRS（逗号分隔地址）与可选的 REDIS_PASSWORD，
// 均未设置时由 test.RunOnRedisCluster 跳过，保证本地/CI 离线测试套件不受影响。
func TestClusterIntegration(t *testing.T) {
	test.RunOnRedisCluster(t, func(rdb redis.Client) {
		addrs := clusterAddrs()
		pwd := clusterPassword()

		// 测试内额外构建 client 所需的连接 Option（与 test 包组装方式等价）
		baseOpts := []redis.Option{redis.WithAddrs(addrs)}
		if pwd != "" {
			baseOpts = append(baseOpts, redis.WithPassword(pwd))
		}

		ctx := context.Background()

		t.Run("集群连接与模式", func(t *testing.T) {
			require.NoError(t, rdb.Ping(ctx).Err(), "集群 Ping 应成功")
			assert.Equal(t, redis.ModeCluster, rdb.Mode(), "运行模式应为 cluster")
			t.Logf("集群地址: %s", strings.Join(addrs, ","))
		})

		t.Run("前缀在集群重定向下正确", func(t *testing.T) {
			// 无前缀 client：交叉验证原始 key（itest:<key>）确实带前缀写入
			plain := redis.New(baseOpts...)
			defer func() { _ = plain.GracefulClose(context.Background()) }()

			prefixed := redis.New(append(baseOpts, redis.WithPrefix("itest"))...)
			defer func() { _ = prefixed.GracefulClose(context.Background()) }()

			// hash tag（{user}:N）与非 hash tag（a/b/c/item:100）混合，
			// 覆盖多个不同 hash slot，触发集群重定向路径。
			keys := []string{"a", "b", "c", "{user}:1", "{user}:2", "item:100", "k1", "k2", "k3", "k4"}
			for i, k := range keys {
				val := fmt.Sprintf("v%d", i)
				require.NoError(t, prefixed.Set(ctx, k, val, time.Hour).Err(), "带前缀写入 %s", k)

				// 无前缀 client 读原始 key "itest:<key>"：前缀只加一次
				got, err := plain.Get(ctx, "itest:"+k).Result()
				require.NoError(t, err, "无前缀读 itest:%s", k)
				assert.Equal(t, val, got, "itest:%s 的值应一致（前缀只加一次）", k)

				// 未加前缀的原始 key 不应存在
				_, err = plain.Get(ctx, k).Result()
				assert.Error(t, err, "原始 key %s 不应存在（前缀必须生效）", k)
			}
		})

		t.Run("LoadFunction 全主节点加载", func(t *testing.T) {
			// FUNCTION 需要 Redis >= 7.0；集群各节点版本一致，探测任一即可
			if !rdb.Capability().VersionAtLeast("7.0") {
				t.Skipf("服务器版本 %s 低于 7.0，不支持 FUNCTION 命令", rdb.Capability().Version())
			}

			// 库名带随机后缀，避免与集群上已有函数冲突
			libName := fmt.Sprintf("itestlib_%s", randomHex(8))
			code := fmt.Sprintf(
				"#!lua name=%s\nredis.register_function('%s', function(keys, args) return args[1] end)",
				libName, "echo_"+libName)

			// 用本库 LoadFunction 加载：集群分支内部 ForEachMaster 分发到所有主节点。
			// 测试结束不删除函数（函数库名唯一带随机后缀，不影响他人；需要清理时
			// 可对每个主节点执行 FUNCTION DELETE）。
			require.NoError(t, rdb.LoadFunction(code), "LoadFunction 应成功")

			// 用独立 goredis ClusterClient 遍历每个主节点执行 FUNCTION LIST，
			// 直接验证每个主节点都加载了该函数库（LoadFunction 集群修复的回归验证）
			// 注意：ForEachMaster 并发遍历各主节点，计数器须用 atomic。
			gc := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: addrs, Password: pwd})
			defer func() { _ = gc.Close() }()

			var masterCount int32
			err := gc.ForEachMaster(ctx, func(mctx context.Context, c *goredis.Client) error {
				atomic.AddInt32(&masterCount, 1)
				libs, err := c.FunctionList(mctx, goredis.FunctionListQuery{LibraryNamePattern: libName}).Result()
				if err != nil {
					return fmt.Errorf("主节点 FUNCTION LIST 失败: %w", err)
				}
				if len(libs) != 1 {
					return fmt.Errorf("主节点未加载函数库 %s（实际命中 %d 个库）", libName, len(libs))
				}
				t.Logf("主节点 %s 已加载函数库 %s", c.Options().Addr, libName)
				return nil
			})
			require.NoError(t, err, "所有主节点都应加载函数库 %s", libName)
			assert.GreaterOrEqual(t, atomic.LoadInt32(&masterCount), int32(1), "应至少遍历到一个主节点")
		})

		t.Run("Capability 探测", func(t *testing.T) {
			require.NoError(t, rdb.Capability().Probe(ctx), "Capability Probe 不应报错")
			ver := rdb.Capability().Version()
			assert.NotEmpty(t, ver, "版本信息不应为空")
			t.Logf("集群 Redis 版本: %s", ver)
		})

		t.Run("AddPrefix 在集群下工作", func(t *testing.T) {
			// 父 rdb 无前缀，子池前缀为 "sub"；子池连接配置继承自父（同一集群）
			sub := rdb.AddPrefix("sub")
			defer func() { _ = sub.GracefulClose(context.Background()) }()

			keys := []string{"a", "{user}:1", "item:100", "k5", "k6"}
			for i, k := range keys {
				val := fmt.Sprintf("subv%d", i)
				require.NoError(t, sub.Set(ctx, k, val, time.Hour).Err(), "子池写入 %s", k)
				got, err := sub.Get(ctx, k).Result()
				require.NoError(t, err, "子池读取 %s", k)
				assert.Equal(t, val, got, "子池 %s 的值应一致", k)
			}

			// 父 client（无前缀）交叉读子池写入的原始 key "sub:<key>"，验证子池前缀生效
			got, err := rdb.Get(ctx, "sub:a").Result()
			require.NoError(t, err, "父池读 sub:a")
			assert.Equal(t, "subv0", got, "子池写入应落在 sub:a")
		})
	})
}
