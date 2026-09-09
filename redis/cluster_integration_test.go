package redis_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/redis/test"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clusterOptions 从环境变量 REDIS_CLUSTER（完整集群 URL）解析出连接配置
// （节点地址 + userinfo 中的密码），供 LoadFunction 等子测试构建独立
// goredis ClusterClient 使用；连接建立与 Skip 守卫统一走 test.RunOnRedisCluster。
func clusterOptions() redis.RedisOptions {
	raw := os.Getenv("REDIS_CLUSTER")
	if raw == "" {
		return redis.RedisOptions{}
	}
	opt, err := redis.ParseURL(raw)
	if err != nil {
		return redis.RedisOptions{}
	}
	return opt
}

// TestClusterIntegration 真实 Redis Cluster 集成验证。
// 覆盖：集群连接与模式判断、前缀在集群重定向下只加一次、LoadFunction
// 全主节点加载、Capability 探测、AddPrefix 派生子池在集群下的读写。
// 需要环境变量 REDIS_CLUSTER（完整集群 URL，含密码），
// 未设置时由 test.RunOnRedisCluster 跳过，保证本地/CI 离线测试套件不受影响。
func TestClusterIntegration(t *testing.T) {
	test.RunOnRedisCluster(t, func(rdb redis.Client) {
		copt := clusterOptions()
		addrs := copt.Addrs

		// 测试内额外构建 client：直接使用 REDIS_CLUSTER 原始 URL（密码在
		// userinfo 中）；WithAddrs 不带密码，在 URL 含密码的集群上会认证失败。
		raw := os.Getenv("REDIS_CLUSTER")

		ctx := context.Background()

		t.Run("集群连接与模式", func(t *testing.T) {
			require.NoError(t, rdb.Ping(ctx).Err(), "集群 Ping 应成功")
			assert.Equal(t, redis.ModeCluster, rdb.Mode(), "运行模式应为 cluster")
			t.Logf("集群地址: %s", strings.Join(addrs, ","))
		})

		t.Run("前缀在集群重定向下正确", func(t *testing.T) {
			// 无前缀 client：交叉验证原始 key（itest:<key>）确实带前缀写入
			plain, err := redis.NewWithUrl(raw)
			require.NoError(t, err, "构建无前缀 client 失败")
			defer func() { _ = plain.GracefulClose(context.Background()) }()

			prefixed, err := redis.NewWithUrl(raw, redis.WithPrefix("itest"))
			require.NoError(t, err, "构建带前缀 client 失败")
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
			gc := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: addrs, Password: copt.Password})
			defer func() { _ = gc.Close() }()

			var masterCount atomic.Int32
			err := gc.ForEachMaster(ctx, func(mctx context.Context, c *goredis.Client) error {
				masterCount.Add(1)
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
			assert.GreaterOrEqual(t, masterCount.Load(), int32(1), "应至少遍历到一个主节点")
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

		t.Run("布隆过滤器集群分片", func(t *testing.T) {
			// 规格 10c：真实集群上验证——
			//   1. 分片 AddMulti/ExistsMulti/单条 Add/Exists 不触发 CROSSSLOT
			//      （各分片键独立路由，由 go-redis 按整键分发）；
			//   2. 分片键 <base>#<idx> 经 CLUSTER KEYSLOT + ClusterShards 验证
			//      散布到多 slot、多节点。
			// 总容量 100000（≥ 8×1000，effectiveN=8）、显式 WithShardCount(8)。
			base := fmt.Sprintf("bloomctest:%s", randomHex(6))
			keys := make([]string, 0, 8)
			for i := range 8 {
				keys = append(keys, fmt.Sprintf("%s#%d", base, i))
			}
			defer func() {
				// 共享实例纪律：仅清理本用例的分片键，严禁 FLUSHDB。
				// 逐键 Del——集群下多键 DEL 跨 slot 会 CROSSSLOT 静默失败。
				for _, k := range keys {
					_ = rdb.Del(ctx, k).Err()
				}
			}()

			bf := rdb.NewBloomFilter(base,
				redis.WithCapacity(100_000), redis.WithFalsePositive(0.01),
				redis.WithShardCount(8))

			items := make([]any, 300)
			for i := range items {
				items[i] = fmt.Sprintf("bloomct-%s-%d", base, i)
			}

			added, err := bf.AddMulti(ctx, items...)
			require.NoError(t, err, "分片 AddMulti 不应报 CROSSSLOT/路由错误")
			require.Len(t, added, len(items))
			for i, v := range added {
				require.True(t, v, "全新 item %s AddMulti 应为 true", items[i])
			}

			// 跨分片查询：已灌入项必为 true（布隆无假阴性），顺序与入参一一对应
			exists, err := bf.ExistsMulti(ctx, items...)
			require.NoError(t, err, "分片 ExistsMulti 失败")
			require.Len(t, exists, len(items))
			for i, v := range exists {
				require.True(t, v, "ExistsMulti[%d]（%s）出现假阴性（回填错位或分片丢写）", i, items[i])
			}

			// 单条路径（Add/Exists → base#idx 单键命令）
			addedOne, err := bf.Add(ctx, "bloomct-single")
			require.NoError(t, err, "分片单条 Add 失败")
			assert.True(t, addedOne, "全新单条 Add 应为 true")
			ok, err := bf.Exists(ctx, "bloomct-single")
			require.NoError(t, err, "分片单条 Exists 失败")
			assert.True(t, ok, "刚 Add 的 item Exists 应为 true")

			// Info 聚合全部分片（BF.* 路径含空分片归一；bitmap 路径天然零值和）
			info, err := bf.Info(ctx)
			require.NoError(t, err, "分片 Info 聚合失败")
			assert.Equal(t, int64(100_000), info.Capacity, "Info.Capacity 应为配置总容量")
			assert.Positive(t, info.NumItems, "NumItems 聚合应 >0")
			assert.Positive(t, info.Size, "Size 聚合应 >0")

			// 裸 base 键不应存在（集群下键名统一带 #idx 后缀）
			_, err = rdb.Get(ctx, base).Result()
			assert.ErrorIs(t, err, goredis.Nil, "集群下不应出现无后缀的裸 base 键 %s", base)

			// 散布验证：CLUSTER KEYSLOT 逐分片键取 slot（服务端计算，客户端
			// 不引入 CRC16 实现）——8 个分片键应落在多个不同 slot。
			// 样本键名随机（base 带随机后缀），8 键全落同 slot/同节点的概率
			// ≤(1/3)^8 量级；若偶发环境 coincidence 失败，重跑即可。
			slots := make(map[int64]string, len(keys))
			for _, k := range keys {
				slot, err := rdb.Do(ctx, "cluster", "keyslot", k).Int64()
				require.NoError(t, err, "CLUSTER KEYSLOT %s", k)
				slots[slot] = k
			}
			assert.Greater(t, len(slots), 1, "分片键应散布到多个 slot，实际全部落 slot %v", slots)

			// 节点级散布验证：对每个分片键在各主节点做 EXISTS 探测，统计真实
			// 持有节点（不解析 CLUSTER SHARDS 的 announced 节点元数据——容器化
			// 测试集群的 node endpoint 字段可能失真；ForEachMaster 枚举的是
			// go-redis 实际建池的地址，即命令能正确路由使用的节点，可靠）。
			gc := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: addrs, Password: copt.Password})
			defer func() { _ = gc.Close() }()

			var mu sync.Mutex
			keysPerNode := map[string]int{}
			require.NoError(t, gc.ForEachMaster(ctx, func(mctx context.Context, c *goredis.Client) error {
				for _, k := range keys {
					n, err := c.Exists(mctx, k).Result()
					if err != nil {
						// 独立 master client 查询非本节点 key 返回 MOVED 重定向：
						// 属预期噪音，跳过；key 最终会在其 owner 节点上 EXISTS==1。
						if strings.HasPrefix(err.Error(), "MOVED ") {
							continue
						}
						return fmt.Errorf("节点 %s EXISTS %s: %w", c.Options().Addr, k, err)
					}
					if n == 1 {
						mu.Lock()
						keysPerNode[c.Options().Addr]++
						mu.Unlock()
					}
				}
				return nil
			}), "ForEachMaster 探测分片键失败")
			assert.Greater(t, len(keysPerNode), 1, "分片键应散布到多个主节点，实际分布 %v", keysPerNode)
			t.Logf("分片键散布：%d 个 slot、%d 个主节点，节点分布 %v", len(slots), len(keysPerNode), keysPerNode)
		})

		t.Run("布隆 Info 空分片归一", func(t *testing.T) {
			// 评审整改⑤a：真实集群上执行 BF 分片路径 Info 的
			// "BF.INFO not found → 零值分片归一"分支——全空分片（零写入）
			// 时 Info 不得整体报错，聚合为零值；灌入少量元素后聚合转正。
			// BF 路径依赖模块（前置 HasBloom() skip 门保证 auto 恒走 BF）：
			// 无 bf 模块环境按既有守卫风格 skip。
			if !rdb.Capability().HasBloom() {
				t.Skip("集群未加载 bf 模块，跳过 BF 路径 Info 空分片归一验证")
			}
			base := fmt.Sprintf("bloomctest:%s", randomHex(6))
			keys := make([]string, 0, 8)
			for i := range 8 {
				keys = append(keys, fmt.Sprintf("%s#%d", base, i))
			}
			defer func() {
				for _, k := range keys { // 逐键 Del（多键跨 slot 会 CROSSSLOT）
					_ = rdb.Del(ctx, k).Err()
				}
			}()

			bf := rdb.NewBloomFilter(base,
				redis.WithCapacity(100_000), redis.WithFalsePositive(0.01),
				redis.WithShardCount(8))

			// 零写入：8 个分片键全部未初始化，逐分片 "not found" 归一
			info, err := bf.Info(ctx)
			require.NoError(t, err, "全空分片的 Info 应归一为零值而非整体报错")
			assert.Equal(t, int64(0), info.NumItems, "空分片聚合 NumItems 应为 0")
			assert.Equal(t, int64(0), info.Capacity, "空分片聚合 Capacity 应为 0")
			assert.Equal(t, int64(0), info.Size, "空分片聚合 Size 应为 0")
			assert.Equal(t, int64(0), info.NumFilters, "空分片聚合 NumFilters 应为 0")

			// 灌入后聚合转正（分片态 BF 路径逐片惰性 BF.RESERVE）
			seed := make([]any, 30)
			for i := range seed {
				seed[i] = fmt.Sprintf("emptynorm-%s-%d", base, i)
			}
			_, err = bf.AddMulti(ctx, seed...)
			require.NoError(t, err, "BF 分片 AddMulti 失败")
			info2, err := bf.Info(ctx)
			require.NoError(t, err)
			assert.Greater(t, info2.NumItems, int64(0), "灌入后 NumItems 应 >0")
			assert.Greater(t, info2.Capacity, int64(0), "已初始化分片应贡献 Capacity")
			assert.GreaterOrEqual(t, info2.NumFilters, int64(1), "已初始化分片应贡献 NumFilters")
		})
	})
}
