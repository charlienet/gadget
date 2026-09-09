package redis_test

import (
	"context"
	"testing"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/redis/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCuckooFilter 验证布谷鸟过滤器（CF.* 命令族）。
// miniredis 不支持 CF.* 命令，需真实 Redis + RedisBloom 的 cuckoo 模块；
// 服务器未加载 cuckoo 模块（Capability().HasCuckoo() == false）时跳过。
func TestCuckooFilter(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		if !rdb.Capability().HasCuckoo() {
			t.Skip("服务器未加载 cuckoo 模块，跳过 CF.* 测试（需 RedisBloom）")
		}

		ctx := context.Background()
		key := "cf:test"

		// 指定容量：Add 时惰性 CF.RESERVE 预分配；先删除确保从空过滤器开始
		require.NoError(t, rdb.Del(ctx, key).Err())
		cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(10000))

		t.Run("Add 与 Exists", func(t *testing.T) {
			added, err := cf.Add(ctx, "item1")
			require.NoError(t, err)
			assert.True(t, added, "首次添加应返回新增")

			exists, err := cf.Exists(ctx, "item1")
			require.NoError(t, err)
			assert.True(t, exists)

			// 未添加的元素可能假阳性，但刚创建的空过滤器不应命中
			exists, err = cf.Exists(ctx, "never-added")
			require.NoError(t, err)
			assert.False(t, exists)
		})

		t.Run("Del 与 Info", func(t *testing.T) {
			deleted, err := cf.Del(ctx, "item1")
			require.NoError(t, err)
			assert.True(t, deleted, "已存在元素删除应成功")

			// 删除后 Exists 应返回 false（布谷鸟过滤器支持删除，无假阴性）
			exists, err := cf.Exists(ctx, "item1")
			require.NoError(t, err)
			assert.False(t, exists, "删除后元素不应再命中")

			info, err := cf.Info(ctx)
			require.NoError(t, err)
			assert.NotNil(t, info)
			assert.Greater(t, info.Size, int64(0), "过滤器应有实际大小")
		})

		t.Run("惰性创建（不指定容量）", func(t *testing.T) {
			cf2 := rdb.NewCuckooFilter("cf:test2")
			require.NoError(t, rdb.Del(ctx, "cf:test2").Err())

			added, err := cf2.Add(ctx, "x")
			require.NoError(t, err)
			assert.True(t, added, "未预分配时 CF.ADD 应惰性创建过滤器")
		})
	})
}

// TestCuckooFilterReset 验证模块版（CF.* 原生）Reset：整键销毁、幂等，
// 以及关键探针——Reset 复位 CF.RESERVE 闸门后，继续 Add 按 With* 配置
// 重发 RESERVE 重建（未复位则 CF.ADD 惰性创建将被模块默认参数
// bucketSize=2 隐式重建，WithBucketSize 静默作废）。
// miniredis 不支持 CF.* 命令，需真实 Redis + RedisBloom 的 cuckoo 模块；
// 环境不满足时跳过（手法与 TestCuckooFilter 一致）。
func TestCuckooFilterReset(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		if !rdb.Capability().HasCuckoo() {
			t.Skip("服务器未加载 cuckoo 模块，跳过 CF.* Reset 测试（需 RedisBloom）")
		}

		ctx := context.Background()
		key := "cf:reset"
		require.NoError(t, rdb.Del(ctx, key).Err())

		// WithBucketSize(3) 刻意偏离模块默认值 2，作为闸门复位的判据
		cf := rdb.NewCuckooFilter(key,
			redis.WithCuckooCapacity(1000),
			redis.WithBucketSize(3),
			redis.WithMaxIterations(20),
		)

		t.Run("Reset 销毁与幂等", func(t *testing.T) {
			added, err := cf.Add(ctx, "item1")
			require.NoError(t, err)
			require.True(t, added)

			info, err := cf.Info(ctx)
			require.NoError(t, err)
			require.Equal(t, int64(3), info.BucketSize, "首次 RESERVE 应按配置建为 3")

			require.NoError(t, cf.Reset(ctx))

			exists, err := rdb.Exists(ctx, key).Result()
			require.NoError(t, err)
			assert.Equal(t, int64(0), exists, "Reset 后物理键应不存在")

			hit, err := cf.Exists(ctx, "item1")
			require.NoError(t, err)
			assert.False(t, hit, "Reset 后原 item 不应命中")

			// 幂等：键不存在时 Reset 无错，可连发
			require.NoError(t, cf.Reset(ctx))
			require.NoError(t, cf.Reset(ctx))
		})

		t.Run("闸门复位探针：Reset 后 Add 按配置重发 RESERVE", func(t *testing.T) {
			// 上一子测试已 Reset（键不存在）。再 Add：若闸门已复位，
			// ensureReserve 重发 CF.RESERVE BUCKET_SIZE 3 → Info.BucketSize==3；
			// 若未复位，once 燃尽跳过 RESERVE，CF.ADD 惰性建默认 bucketSize=2。
			added, err := cf.Add(ctx, "item2")
			require.NoError(t, err)
			assert.True(t, added)

			info, err := cf.Info(ctx)
			require.NoError(t, err)
			assert.Equal(t, int64(3), info.BucketSize,
				"Reset 后应重发 RESERVE 按 WithBucketSize(3) 重建（==2 即闸门复位缺失）")
			assert.Equal(t, int64(20), info.MaxIterations, "MaxIterations 同样应来自重发的 RESERVE")
		})
	})
}

// TestCuckooFilterMultiOps 验证模块版（CF.* 原生）ExistsMulti/Count/AddNX
// 语义，含关键回归：模块版 CF.ADD 是多重集插入（Count 可 >1、AddNX 与
// Add 分叉），与回退版去重语义形成对照。miniredis 不支持 CF.* 命令，需
// 真实 Redis + RedisBloom 的 cuckoo 模块；环境不满足时跳过（手法与
// TestCuckooFilter 一致）。
func TestCuckooFilterMultiOps(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		if !rdb.Capability().HasCuckoo() {
			t.Skip("服务器未加载 cuckoo 模块，跳过 CF.* 批量操作测试（需 RedisBloom）")
		}

		ctx := context.Background()
		key := "cf:multi"

		t.Run("ExistsMulti 顺序与缺失键", func(t *testing.T) {
			require.NoError(t, rdb.Del(ctx, key).Err())
			cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(1000))

			// 只读路径不触发 RESERVE/惰性创建：键不存在时全 false、无错
			res, err := cf.ExistsMulti(ctx, "ghost-1", "ghost-2")
			require.NoError(t, err, "不存在键的 CF.MEXISTS 应全 0 而非报错")
			require.Len(t, res, 2)
			assert.False(t, res[0] || res[1])

			require.NoError(t, cf.Reset(ctx)) // 确保干净起点（上一步若惰性建）
			_, err = cf.Add(ctx, "p-1")
			require.NoError(t, err)
			_, err = cf.Add(ctx, "p-2")
			require.NoError(t, err)

			queries := []any{"p-1", "ghost-a", "p-2", "p-1"}
			got, err := cf.ExistsMulti(ctx, queries...)
			require.NoError(t, err)
			require.Len(t, got, len(queries))
			// 一致性锚定：与逐条 CF.EXISTS 全等
			for i, q := range queries {
				want, err := cf.Exists(ctx, q)
				require.NoError(t, err)
				assert.Equal(t, want, got[i], "ExistsMulti 第 %d 项与单条 Exists 分叉", i)
			}
			assert.True(t, got[0] && got[2] && got[3], "已添加项必命中（无假阴性）")

			// 空入参
			empty, err := cf.ExistsMulti(ctx)
			require.NoError(t, err)
			assert.Nil(t, empty)
		})

		t.Run("Count 多重集语义（模块路径关键回归）", func(t *testing.T) {
			require.NoError(t, rdb.Del(ctx, key).Err())
			cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(1000))

			// CF.ADD 多重集：同一 item 插 3 次全部入桶
			for range 3 {
				_, err := cf.Add(ctx, "dup")
				require.NoError(t, err)
			}
			n, err := cf.Count(ctx, "dup")
			require.NoError(t, err)
			assert.Equal(t, int64(3), n, "模块版 Count 应反映多重集计数（回退版恒 0/1，勿跨路径依赖）")

			del, err := cf.Del(ctx, "dup")
			require.NoError(t, err)
			require.True(t, del)
			n, err = cf.Count(ctx, "dup")
			require.NoError(t, err)
			assert.Equal(t, int64(2), n, "Del 一次计数应减 1（CF 删除精确到实例）")

			// 不存在的键/元素：0 无错
			n, err = cf.Count(ctx, "never")
			require.NoError(t, err)
			assert.Equal(t, int64(0), n)
		})

		t.Run("AddNX 存在即不加（与 Add 差异回归）", func(t *testing.T) {
			require.NoError(t, rdb.Del(ctx, key).Err())
			cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(1000))

			added, err := cf.AddNX(ctx, "nx")
			require.NoError(t, err)
			assert.True(t, added, "首次插入应成功")

			added, err = cf.AddNX(ctx, "nx")
			require.NoError(t, err)
			assert.False(t, added, "AddNX 对已存在元素应不插入")

			// 与 Add 差异：Add 多重集再插一份 → Count 增；AddNX 不增
			ok, err := cf.Add(ctx, "nx")
			require.NoError(t, err)
			assert.True(t, ok, "模块版 Add 已存在也插入（多重集），与回退版分叉")
			n, err := cf.Count(ctx, "nx")
			require.NoError(t, err)
			assert.Equal(t, int64(2), n, "Add 后 Count 应为 2")

			added, err = cf.AddNX(ctx, "nx")
			require.NoError(t, err)
			assert.False(t, added, "存在即不加")
			n, err = cf.Count(ctx, "nx")
			require.NoError(t, err)
			assert.Equal(t, int64(2), n, "AddNX 拒绝时不得增值")
		})

		t.Run("RESERVE 撞已存在过滤器吞错维持武装", func(t *testing.T) {
			// exists 类吞错分支的集成覆盖（miniredis 无法注入
			// "Item already exists" 错误文本，见 internal 注释）：
			// 第二个实例带全新闸门对已存在键 Add——CF.RESERVE 报
			// "Item already exists" → ensureReserve 吞错返回 nil（不解除
			// 武装、不报错），CF.ADD 正常执行。
			require.NoError(t, rdb.Del(ctx, key).Err())
			cf4 := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(1000))
			_, err := cf4.Add(ctx, "first")
			require.NoError(t, err)

			cf5 := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(1000))
			added, err := cf5.Add(ctx, "second")
			require.NoError(t, err, "exists 类 RESERVE 错误应被吞掉，Add 整体成功")
			assert.True(t, added)

			// 吞错视为已消费闸门：后续 Add 正常（不重复 RESERVE 报错）
			added, err = cf5.Add(ctx, "third")
			require.NoError(t, err)
			assert.True(t, added)
		})
	})
}

// TestCuckooFilterAddMulti 验证模块版（CF.* 原生）AddMulti：CF.INSERT
// 单命令批量插入、结果与入参顺序一一对应；关键回归——模块版为**多重集**
// 插入，同一 item 在批量中出现两次则双双 true 且 Count 合计 2（与回退版
// 去重语义分叉）。miniredis 不支持 CF.* 命令，需真实 Redis + RedisBloom
// 的 cuckoo 模块；环境不满足时跳过（手法与 TestCuckooFilter 一致）。
func TestCuckooFilterAddMulti(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		if !rdb.Capability().HasCuckoo() {
			t.Skip("服务器未加载 cuckoo 模块，跳过 CF.* AddMulti 测试（需 RedisBloom）")
		}

		ctx := context.Background()
		key := "cf:addmulti"
		require.NoError(t, rdb.Del(ctx, key).Err())

		// capacity 指定：首个写操作经 ensureReserve 惰性 CF.RESERVE；
		// AddMulti 的 CF.INSERT 不带 CAPACITY/NOCREATE（预分配单一通道）
		cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(1000))

		t.Run("3 项批量顺序对应", func(t *testing.T) {
			res, err := cf.AddMulti(ctx, "a-1", "a-2", "a-3")
			require.NoError(t, err)
			require.Len(t, res, 3)
			assert.True(t, res[0] && res[1] && res[2], "低负载全新键应全部插入成功")

			// 顺序锚定：逐项单条 Exists 核对（无假阴性）
			for i, it := range []string{"a-1", "a-2", "a-3"} {
				hit, err := cf.Exists(ctx, it)
				require.NoError(t, err)
				assert.True(t, hit, "批量插入第 %d 项 %s 应命中", i, it)
			}

			// 空入参惯例
			empty, err := cf.AddMulti(ctx)
			require.NoError(t, err)
			assert.Nil(t, empty, "空入参应返回 (nil, nil)")
		})

		t.Run("多重集关键回归：同 item 批量内出现两次双双 true", func(t *testing.T) {
			res, err := cf.AddMulti(ctx, "b-1", "b-1")
			require.NoError(t, err)
			require.Len(t, res, 2)
			assert.True(t, res[0], "首次 b-1 插入应成功")
			assert.True(t, res[1], "模块版 CF.INSERT 多重集：同 item 第二次出现也插入（回退版此处为 false）")

			n, err := cf.Count(ctx, "b-1")
			require.NoError(t, err)
			assert.Equal(t, int64(2), n, "批量内重复项 Count 合计应为 2")

			// 前序数据不受影响
			n, err = cf.Count(ctx, "a-1")
			require.NoError(t, err)
			assert.Equal(t, int64(1), n)
		})

		t.Run("与 AddNX 分叉", func(t *testing.T) {
			// "c-1" 全新：AddMulti 插入后，AddNX 对同 item 拒绝——
			// AddNX 恒"存在即不加"与批量多重集形成的对照
			res, err := cf.AddMulti(ctx, "c-1")
			require.NoError(t, err)
			require.True(t, res[0])

			added, err := cf.AddNX(ctx, "c-1")
			require.NoError(t, err)
			assert.False(t, added, "AddNX 对已存在元素必须不加")

			n, err := cf.Count(ctx, "c-1")
			require.NoError(t, err)
			assert.Equal(t, int64(1), n)
		})
	})
}
