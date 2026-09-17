package redis_test

import (
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
		_ = rdb.Capability().Probe(t.Context()) // 查询为纯内存读：guard 前显式探测
		if !rdb.Capability().HasCuckoo() {
			t.Skip("服务器未加载 cuckoo 模块，跳过 CF.* 测试（需 RedisBloom）")
		}

		ctx := t.Context()
		key := "cf:test"

		// 指定容量：Add 时惰性 CF.RESERVE 预分配；先删除确保从空过滤器开始
		require.NoError(t, rdb.Del(ctx, key).Err())
		cf, cerr27 := rdb.NewCuckooFilter(t.Context(), key, redis.WithCuckooCapacity(10000))
		if cerr27 != nil {
			t.Fatalf("构造过滤器：%v", cerr27)
		}

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

		t.Run("无 Option 默认 1e6 构造即建键", func(t *testing.T) {
			key2 := "cf:test2:" + randomHex(6)
			defer func() { _ = rdb.Del(ctx, key2).Err() }()
			cf2, cerr61 := rdb.NewCuckooFilter(ctx, key2)
			require.NoError(t, cerr61, "默认档构造（CF.RESERVE 1e6）")
			// New 即建：无写入 CF.INFO 即可用，容量口径为默认 1e6
			info, err := cf2.Info(ctx)
			require.NoError(t, err, "构造后 CF.INFO 立即可用")
			assert.Equal(t, int64(524288), info.NumBuckets, "默认 1e6/2 向上取 2 的幂=2^19")

			added, err := cf2.Add(ctx, "x")
			require.NoError(t, err)
			assert.True(t, added)
		})
	})
}

// TestCuckooFilterReset 验证模块版（CF.* 原生）Reset：整键销毁并同步按
// 当前配置重建，幂等；关键探针——重建后的键参数仍为 With* 显式配置
// （bucketSize=3/maxIterations=20），未走构造期 RESERVE 通道才会回落
// 模块默认值。
// miniredis 不支持 CF.* 命令，需真实 Redis + RedisBloom 的 cuckoo 模块；
// 环境不满足时跳过（手法与 TestCuckooFilter 一致）。
func TestCuckooFilterReset(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		_ = rdb.Capability().Probe(t.Context()) // 查询为纯内存读：guard 前显式探测
		if !rdb.Capability().HasCuckoo() {
			t.Skip("服务器未加载 cuckoo 模块，跳过 CF.* Reset 测试（需 RedisBloom）")
		}

		ctx := t.Context()
		key := "cf:reset"
		require.NoError(t, rdb.Del(ctx, key).Err())

		// WithBucketSize(3) 刻意偏离模块默认值 2，作为构造连接与 Reset
		// 同步重建的参数判据
		cf, cerr := rdb.NewCuckooFilter(ctx, key,
			redis.WithCuckooCapacity(1000),
			redis.WithBucketSize(3),
			redis.WithMaxIterations(20),
		)
		require.NoError(t, cerr, "构造即连接（CF.RESERVE）")

		t.Run("Reset 销毁与同步重建", func(t *testing.T) {
			added, err := cf.Add(ctx, "item1")
			require.NoError(t, err)
			require.True(t, added)

			info, err := cf.Info(ctx)
			require.NoError(t, err)
			require.Equal(t, int64(3), info.BucketSize, "构造期 RESERVE 应按配置建为 3")

			require.NoError(t, cf.Reset(ctx))

			// Reset 同步重建：返回即键已按配置重建存在，且为空过滤器
			exists, err := rdb.Exists(ctx, key).Result()
			require.NoError(t, err)
			assert.Equal(t, int64(1), exists, "Reset 返回后键应已同步重建")

			hit, err := cf.Exists(ctx, "item1")
			require.NoError(t, err)
			assert.False(t, hit, "Reset 后原 item 不应命中")

			// 幂等：Reset 可连发（DEL 与 RESERVE-复用皆幂等）
			require.NoError(t, cf.Reset(ctx))
			require.NoError(t, cf.Reset(ctx))
		})

		t.Run("重建参数探针：Reset 后 Info 保持配置口径", func(t *testing.T) {
			// 上一子测试两次 Reset 均同步重建；bucketSize/maxIterations
			// 仍为显式配置值（3/20），未回落模块默认（2/20）即重建走
			// connectAll 而非惰性写路径。
			added, err := cf.Add(ctx, "item2")
			require.NoError(t, err)
			assert.True(t, added)

			info, err := cf.Info(ctx)
			require.NoError(t, err)
			assert.Equal(t, int64(3), info.BucketSize,
				"Reset 重建按 WithBucketSize(3)（==2 即重建参数丢失）")
			assert.Equal(t, int64(20), info.MaxIterations)
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
		_ = rdb.Capability().Probe(t.Context()) // 查询为纯内存读：guard 前显式探测
		if !rdb.Capability().HasCuckoo() {
			t.Skip("服务器未加载 cuckoo 模块，跳过 CF.* 批量操作测试（需 RedisBloom）")
		}

		ctx := t.Context()
		key := "cf:multi"

		t.Run("ExistsMulti 顺序与缺失键", func(t *testing.T) {
			require.NoError(t, rdb.Del(ctx, key).Err())
			cf, cerr153 := rdb.NewCuckooFilter(ctx, key, redis.WithCuckooCapacity(1000))
			if cerr153 != nil {
				t.Fatalf("构造过滤器：%v", cerr153)
			}

			// 只读路径不触发 RESERVE：键不存在时全 false、无错
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
			cf, cerr187 := rdb.NewCuckooFilter(ctx, key, redis.WithCuckooCapacity(1000))
			if cerr187 != nil {
				t.Fatalf("构造过滤器：%v", cerr187)
			}

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
			cf, cerr213 := rdb.NewCuckooFilter(ctx, key, redis.WithCuckooCapacity(1000))
			if cerr213 != nil {
				t.Fatalf("构造过滤器：%v", cerr213)
			}

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
			// "Item already exists" → 连接期吞错返回 nil（不解除
			// 武装、不报错），CF.ADD 正常执行。
			require.NoError(t, rdb.Del(ctx, key).Err())
			cf4, cerr246 := rdb.NewCuckooFilter(ctx, key, redis.WithCuckooCapacity(1000))
			if cerr246 != nil {
				t.Fatalf("构造过滤器：%v", cerr246)
			}
			_, err := cf4.Add(ctx, "first")
			require.NoError(t, err)

			cf5, cerr250 := rdb.NewCuckooFilter(ctx, key, redis.WithCuckooCapacity(1000))
			if cerr250 != nil {
				t.Fatalf("构造过滤器：%v", cerr250)
			}
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
		_ = rdb.Capability().Probe(t.Context()) // 查询为纯内存读：guard 前显式探测
		if !rdb.Capability().HasCuckoo() {
			t.Skip("服务器未加载 cuckoo 模块，跳过 CF.* AddMulti 测试（需 RedisBloom）")
		}

		ctx := t.Context()
		key := "cf:addmulti"
		require.NoError(t, rdb.Del(ctx, key).Err())

		// capacity 指定：构造期 CF.RESERVE 已建键（连接语义）；
		// AddMulti 的 CF.INSERT 不带 CAPACITY/NOCREATE（预分配单一通道）
		cf, cerr281 := rdb.NewCuckooFilter(t.Context(), key, redis.WithCuckooCapacity(1000))
		if cerr281 != nil {
			t.Fatalf("构造过滤器：%v", cerr281)
		}

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

// TestCuckooFactoryReal 真实单机 RedisBloom 上 CF.* 工厂"构造即连接"锚：
// 默认档构造即建键（CF.INFO 立即可用，实测 NumBuckets=2^19=524288——
// 1e6/bucketSize(2) 向上取 2 的幂）；既有真 CF 键复用不改写；显式参数
// 不符（capacity 容纳不足 / bucketSize 不等）构造报 layout mismatch 且
// 键未触碰；Reset 按新参数同步重建。环境守卫与键纪律同其他真机用例。
func TestCuckooFactoryReal(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		_ = rdb.Capability().Probe(t.Context()) // 查询为纯内存读：guard 前显式探测
		if !rdb.Capability().HasCuckoo() {
			t.Skip("服务器未加载 cuckoo 模块，跳过 CF 工厂连接锚")
		}
		ctx := t.Context()
		keyOf := func(label string) string { return "cfrs:" + randomHex(6) + ":" + label }
		del := func(key string) { _ = rdb.Del(ctx, key).Err() }

		t.Run("默认档构造即建键", func(t *testing.T) {
			key := keyOf("dflt")
			defer del(key)
			cf, err := rdb.NewCuckooFilter(ctx, key)
			require.NoError(t, err, "默认档构造（CF.RESERVE 1e6）")
			// New 即建：无写入 CF.INFO 即可用
			info, err := cf.Info(ctx)
			require.NoError(t, err, "构造后 CF.INFO 应立即可用")
			assert.Equal(t, int64(524288), info.NumBuckets, "实录：1e6/2 向上取 2 的幂=2^19")
			assert.Equal(t, int64(2), info.BucketSize)
			assert.Equal(t, int64(0), info.NumItems)
		})

		t.Run("既有真CF键复用不改写", func(t *testing.T) {
			key := keyOf("reuse")
			defer del(key)
			require.NoError(t, rdb.CFReserve(ctx, key, 1000).Err())
			// 期望容量 1000 ≤ 服务端槽位（512×2=1024）：容量口径容纳即复用
			cf, err := rdb.NewCuckooFilter(ctx, key, redis.WithCuckooCapacity(1000))
			require.NoError(t, err, "容纳充分的既有键应复用成功")
			info, err := cf.Info(ctx)
			require.NoError(t, err)
			assert.Equal(t, int64(512), info.NumBuckets, "复用不改写既有桶数")
		})

		t.Run("capacity不符构造报错键未触碰", func(t *testing.T) {
			key := keyOf("capmis")
			defer del(key)
			require.NoError(t, rdb.CFReserve(ctx, key, 100).Err()) // 64×2=128 槽位
			_, err := rdb.NewCuckooFilter(ctx, key, redis.WithCuckooCapacity(1000000))
			require.Error(t, err, "既有键容纳不足应报 layout mismatch")
			assert.Contains(t, err.Error(), "layout mismatch")
			assert.NotErrorIs(t, err, redis.ErrRedisUnavailable, "数据类错误不得包哨兵")
			info, ierr := rdb.CFInfo(ctx, key).Result()
			require.NoError(t, ierr)
			assert.Equal(t, int64(64), info.NumBuckets, "mismatch 不得触碰既有键")
		})

		t.Run("bucketSize不符构造报错", func(t *testing.T) {
			key := keyOf("bsmis")
			defer del(key)
			require.NoError(t, rdb.CFReserve(ctx, key, 1000).Err()) // 服务端 bucketSize 默认 2
			_, err := rdb.NewCuckooFilter(ctx, key,
				redis.WithCuckooCapacity(1000), redis.WithBucketSize(3))
			require.Error(t, err, "显式 bucketSize 与服务端不符应报错")
			assert.Contains(t, err.Error(), "bucket_size")
		})

		t.Run("Reset按实例配置同步重建", func(t *testing.T) {
			key := keyOf("reset")
			defer del(key)
			require.NoError(t, rdb.CFReserve(ctx, key, 100).Err()) // 64×2=128 槽位
			// 默认档 1e6 与既有 128 槽位容纳不足 → 构造期 layout mismatch
			_, err := rdb.NewCuckooFilter(ctx, key)
			require.Error(t, err, "既有键容纳不足应报错（默认 1e6 口径参与比对）")
			assert.Contains(t, err.Error(), "layout mismatch")
			// 显式 capacity=100 实例复用构造 → Add → Reset 按该实例 cfg
			// 重新 RESERVE 重建（64 桶、空过滤器）
			cf2, err2 := rdb.NewCuckooFilter(ctx, key, redis.WithCuckooCapacity(100))
			require.NoError(t, err2, "capacity=100 ≤ 128 槽位应复用")
			_, e := cf2.Add(ctx, "z-1")
			require.NoError(t, e)
			require.NoError(t, cf2.Reset(ctx))
			info, err := cf2.Info(ctx)
			require.NoError(t, err, "Reset 同步重建后 CF.INFO 可用")
			assert.Equal(t, int64(64), info.NumBuckets, "Reset 按实例 cfg capacity=100 重建")
			assert.Equal(t, int64(0), info.NumItems, "重建后为空过滤器")
		})
	})
}
