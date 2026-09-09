package redis_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/redis/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCuckooFilterCluster 在真实 Redis Cluster（6 节点、加载 RedisBloom）上
// 跑通 cfCmdImpl 原生路径的全部 9 个门面方法（Add/Exists/Del/Info/Reset/
// ExistsMulti/AddMulti/Count/AddNX），关键断言与真单机用例（cuckoo_test.go）
// 对齐：
//   - Reset 后 Info 的 not-found 行为（命令级错误原样透传、不判 Unavailable）；
//   - Count 多重集回归（Add×3 → 3、Del 一次 → 2）；
//   - 闸门复位探针（WithBucketSize(3) + Reset → Add → Info BucketSize==3）；
//   - AddNX 与 Add 的分叉（NX 存在即不加 vs Add 多重集再插）。
//
// CF.* 均为单键命令，集群上无 CROSSSLOT 形态；key 带唯一纳秒段避免共享
// 实例冲突，收尾 Del、严禁 FLUSHDB。REDIS_CLUSTER 未设置或节点无 cuckoo
// 模块时跳过（守卫手法与 TestCuckooFilter/TestCuckooFilterMultiOps 一致）。
func TestCuckooFilterCluster(t *testing.T) {
	test.RunOnRedisCluster(t, func(rdb redis.Client) {
		if !rdb.Capability().HasCuckoo() {
			t.Skip("集群节点未加载 cuckoo 模块，跳过 CF.* 集群测试（需 RedisBloom）")
		}

		ctx := context.Background()
		key := fmt.Sprintf("cf:cluster:%d", time.Now().UnixNano())
		t.Cleanup(func() { _ = rdb.Del(ctx, key).Err() })

		// WithBucketSize(3)/WithMaxIterations(20) 刻意偏离模块默认（2/20），
		// 供闸门复位探针判据
		cf := rdb.NewCuckooFilter(key,
			redis.WithCuckooCapacity(10000),
			redis.WithBucketSize(3),
			redis.WithMaxIterations(20),
		)

		t.Run("全 9 方法门面级跑通", func(t *testing.T) {
			// Add / Exists
			added, err := cf.Add(ctx, "c-1")
			require.NoError(t, err)
			assert.True(t, added)
			hit, err := cf.Exists(ctx, "c-1")
			require.NoError(t, err)
			assert.True(t, hit)

			// AddNX
			added, err = cf.AddNX(ctx, "nx-1")
			require.NoError(t, err)
			assert.True(t, added, "AddNX 首次应插入（闸门 RESERVE 已在 Add 时消费）")
			added, err = cf.AddNX(ctx, "nx-1")
			require.NoError(t, err)
			assert.False(t, added, "AddNX 重复必须不加")

			// AddMulti（CF.INSERT 多重集：重复项两次都插）
			res, err := cf.AddMulti(ctx, "m-1", "m-1", "m-2")
			require.NoError(t, err)
			require.Len(t, res, 3)
			assert.True(t, res[0] && res[1] && res[2], "集群 CF.INSERT 多重集语义：批量内重复项双双插入")

			// ExistsMulti 顺序锚定（与逐条 Exists 全等）
			queries := []any{"c-1", "ghost-1", "m-1", "nx-1"}
			got, err := cf.ExistsMulti(ctx, queries...)
			require.NoError(t, err)
			require.Len(t, got, len(queries))
			for i, q := range queries {
				want, err := cf.Exists(ctx, q)
				require.NoError(t, err)
				assert.Equal(t, want, got[i], "ExistsMulti 第 %d 项与逐条 Exists 分叉", i)
			}
			assert.True(t, got[0] && got[2] && got[3], "已插入项必命中（无假阴性）")

			// Count
			n, err := cf.Count(ctx, "m-1")
			require.NoError(t, err)
			assert.Equal(t, int64(2), n, "AddMulti 内重复两项 → Count 2")

			// Del：键存在但元素从未插入 → (false, nil)，无错
			del, err := cf.Del(ctx, "no-such-item")
			require.NoError(t, err, "键存在时 CF.DEL 未命中元素应返回 0 而非报错")
			assert.False(t, del)

			// Del 单副本元素 → true，再查 false（c-1 未经 AddMulti 重复，恒单副本）
			del, err = cf.Del(ctx, "c-1")
			require.NoError(t, err)
			assert.True(t, del)
			hit, err = cf.Exists(ctx, "c-1")
			require.NoError(t, err)
			assert.False(t, hit, "Del 后不应命中（CF 删除精确）")

			// Info
			info, err := cf.Info(ctx)
			require.NoError(t, err)
			assert.Greater(t, info.NumBuckets, int64(0))
			assert.Equal(t, int64(3), info.BucketSize, "RESERVE 参数在集群上应生效")
		})

		t.Run("Count 多重集回归（Add×3 → 3、Del 一次 → 2）", func(t *testing.T) {
			require.NoError(t, rdb.Del(ctx, key).Err())

			// 全新键首个写操作重走惰性 CF.RESERVE（DEL 未过 Reset，实例
			// 闸门仍处上代消费态——用新实例确保 RESERVE 通道完整验证）
			cf2 := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(10000))
			for range 3 {
				_, err := cf2.Add(ctx, "dup")
				require.NoError(t, err)
			}
			n, err := cf2.Count(ctx, "dup")
			require.NoError(t, err)
			assert.Equal(t, int64(3), n, "模块版 Count 反映多重集计数")

			del, err := cf2.Del(ctx, "dup")
			require.NoError(t, err)
			require.True(t, del)
			n, err = cf2.Count(ctx, "dup")
			require.NoError(t, err)
			assert.Equal(t, int64(2), n, "Del 一次计数减 1")
		})

		t.Run("Reset 后 Info not-found 透传与幂等", func(t *testing.T) {
			require.NoError(t, rdb.Del(ctx, key).Err())
			cf3 := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(10000))
			_, err := cf3.Add(ctx, "r-1")
			require.NoError(t, err)

			require.NoError(t, cf3.Reset(ctx))

			exists, err := rdb.Exists(ctx, key).Result()
			require.NoError(t, err)
			assert.Equal(t, int64(0), exists, "Reset 后物理键应删除")

			// CF.INFO 对不存在键报命令级错误（"not found"类）：门面原样
			// 透传错误、**不得**判为 ErrRedisUnavailable（服务实际可用，
			// 与真单机行为对齐）。
			_, err = cf3.Info(ctx)
			require.Error(t, err, "Reset 后 CF.INFO 应报 not-found 类命令错误")
			assert.NotErrorIs(t, err, redis.ErrRedisUnavailable,
				"not-found 是命令级错误，不得触发 Unavailable 判定")

			// Exists 对已销毁键返回 false 无错（CF.EXISTS 缺失键宽容形态）
			hit, err := cf3.Exists(ctx, "r-1")
			require.NoError(t, err)
			assert.False(t, hit)

			// ⚠️ 归一化契约锚（评审 M1）：CF.DEL 对不存在键原生报
			// "Not found" 命令错误（缺陷候选被裁决归一），门面将其
			// 归一为 (false, nil)——与回退版 hashImpl（DEL 通用命令
			// 天然幂等）跨路径语义对齐，Del/Reset 组合场景调用方无需
			// 按能力分派处理错误。底层 cfCmdImpl.Del 保留原生报错
			// （白盒可观测）。
			del, err := cf3.Del(ctx, "r-1")
			require.NoError(t, err, "门面 Del 对不存在键应归一为无错")
			assert.False(t, del, "归一化后返回 (false, nil)")

			// 幂等：连发 Reset 无错
			require.NoError(t, cf3.Reset(ctx))
			require.NoError(t, cf3.Reset(ctx))
		})

		t.Run("闸门复位探针：Reset 后 Add 按配置重发 RESERVE", func(t *testing.T) {
			// 复用 cf 实例（构造带 WithBucketSize(3)+WithMaxIterations(20)）；
			// 上个子测试已把键清掉且实例换过，这里从显式状态重建：
			// cf 的闸门在更早子测试已被 Reset——本 sub 直接 Reset 归零世代，
			// 再 Add 触发按配置 RESERVE。
			require.NoError(t, rdb.Del(ctx, key).Err())
			require.NoError(t, cf.Reset(ctx))

			added, err := cf.Add(ctx, "gate-1")
			require.NoError(t, err)
			require.True(t, added)

			info, err := cf.Info(ctx)
			require.NoError(t, err)
			assert.Equal(t, int64(3), info.BucketSize,
				"集群上 Reset 后闸门应复位并按 WithBucketSize(3) 重发 RESERVE（==2 即复位缺失）")
			assert.Equal(t, int64(20), info.MaxIterations, "MaxIterations 来自重发的 RESERVE")
		})

		t.Run("AddNX 与 Add 分叉", func(t *testing.T) {
			require.NoError(t, rdb.Del(ctx, key).Err())
			require.NoError(t, cf.Reset(ctx))

			added, err := cf.AddNX(ctx, "fx")
			require.NoError(t, err)
			require.True(t, added)

			added, err = cf.AddNX(ctx, "fx")
			require.NoError(t, err)
			assert.False(t, added, "AddNX 存在即不加")

			ok, err := cf.Add(ctx, "fx")
			require.NoError(t, err)
			assert.True(t, ok, "模块版 Add 已存在也再插一份（多重集）")

			n, err := cf.Count(ctx, "fx")
			require.NoError(t, err)
			assert.Equal(t, int64(2), n, "Add 使 Count 增值而 AddNX 不能")

			added, err = cf.AddNX(ctx, "fx")
			require.NoError(t, err)
			assert.False(t, added)
			n, err = cf.Count(ctx, "fx")
			require.NoError(t, err)
			assert.Equal(t, int64(2), n, "AddNX 拒绝时不得增值")
		})
	})
}
