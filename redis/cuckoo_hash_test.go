package redis_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/charlienet/gadget/redis"
	mini "github.com/charlienet/gadget/redis/test/mini"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/xxh3"
)

// cuckooFingerprint 复刻 hashImpl 的指纹计算（xxh3.Hash & 0xFFFF，0 取 1），
// 供测试区分"放错位置"与"驱逐丢失"。string 入参经 marshalItem 与
// []byte(item) 同字节（见 marshal.go），故此处直接取字节。
func cuckooFingerprint(item string) int64 {
	h := xxh3.Hash([]byte(item))
	fp := int64(h & 0xFFFF)
	if fp == 0 {
		fp = 1
	}
	return fp
}

// cuckooFingerprintInAnyBucket 检查指纹是否存在于任意桶中（桶 value 为
// 3 字节槽编码 [指纹低字节, 指纹高字节, 方向位]）。
func cuckooFingerprintInAnyBucket(buckets map[string]string, fp int64) bool {
	lo, hi := byte(fp&0xFF), byte((fp>>8)&0xFF)
	for _, val := range buckets {
		for j := 0; j+2 < len(val); j += 3 {
			if val[j] == lo && val[j+1] == hi {
				return true
			}
		}
	}
	return false
}

// TestCuckooHashImpl 验证无模块回退实现（miniredis 无 cuckoo 模块，
// NewCuckooFilter 自动分派到 hashImpl；miniredis 支持 Lua 与 Hash 操作）。
// 覆盖：Add/Exists/Del/Info、幂等语义、驱逐路径与模块版行为对齐。
func TestCuckooHashImpl(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		ctx := context.Background()
		cf := rdb.NewCuckooFilter("cfh:1", redis.WithCuckooCapacity(1000))
		require.NoError(t, rdb.Del(ctx, "cfh:1").Err())

		t.Run("Add 与 Exists 命中与未命中", func(t *testing.T) {
			added, err := cf.Add(ctx, "item1")
			require.NoError(t, err)
			assert.True(t, added, "首次添加应返回新增")

			exists, err := cf.Exists(ctx, "item1")
			require.NoError(t, err)
			assert.True(t, exists, "添加后应命中")

			// 未添加元素不应命中（确定性哈希下误判极低）
			exists, err = cf.Exists(ctx, "never-added")
			require.NoError(t, err)
			assert.False(t, exists, "未添加元素不应命中")
		})

		t.Run("重复 Add 幂等（对齐 CF.ADD 返回 0）", func(t *testing.T) {
			added, err := cf.Add(ctx, "item1")
			require.NoError(t, err)
			assert.False(t, added, "已存在元素重复添加应返回 false")
		})

		t.Run("Del 后 Exists 返回 false", func(t *testing.T) {
			deleted, err := cf.Del(ctx, "item1")
			require.NoError(t, err)
			assert.True(t, deleted, "已存在元素删除应成功")

			exists, err := cf.Exists(ctx, "item1")
			require.NoError(t, err)
			assert.False(t, exists, "删除后不应命中")

			// 删除不存在的元素返回 false（对齐 CF.DEL）
			deleted, err = cf.Del(ctx, "item1")
			require.NoError(t, err)
			assert.False(t, deleted, "删除不存在的元素应返回 false")
		})

		t.Run("驱逐路径：小容量批量插入", func(t *testing.T) {
			small := rdb.NewCuckooFilter("cfh:2", redis.WithCuckooCapacity(100))
			require.NoError(t, rdb.Del(ctx, "cfh:2").Err())

			// capacity=100, bucketSize=4 → 25 桶 × 4 槽 = 100 槽位；
			// 插入 150 个不同元素，超过容量触发驱逐置换路径。
			total := 150
			addedItems := make([]string, 0, total)
			rejected := 0
			for i := range total {
				item := fmt.Sprintf("evict-%d", i)
				added, err := small.Add(ctx, item)
				require.NoError(t, err)
				if added {
					addedItems = append(addedItems, item)
				} else {
					rejected++
				}
			}

			// 分类"Add 成功但查不到"的元素：
			//   - 放错（misplaced）：指纹仍在桶中但位于非候选桶——方向错误，禁止出现
			//   - 丢失（missing）：指纹完全不在任何桶——驱逐链超限被挤出，
			//     cuckoo 超载的正常行为（与 CF.ADD 满时元素被驱逐一致），允许
			allBuckets, err := rdb.HGetAll(ctx, "cfh:2").Result()
			require.NoError(t, err)

			missing, misplaced := 0, 0
			for _, item := range addedItems {
				exists, err := small.Exists(ctx, item)
				require.NoError(t, err)
				if exists {
					continue
				}
				if cuckooFingerprintInAnyBucket(allBuckets, cuckooFingerprint(item)) {
					misplaced++
				} else {
					missing++
				}
			}
			t.Logf("插入 %d 个元素：Add 拒绝 %d 个（超载）、驱逐丢失 %d 个（cuckoo 正常）、放错 %d 个",
				total, rejected, missing, misplaced)

			// 核心断言：方向位驱逐链 + 2 字节指纹保证无"放错"（方向错误假阴性）
			assert.Zero(t, misplaced, "不应有放错位置的指纹（方向错误导致的假阴性）")
			assert.Greater(t, len(addedItems), 0, "应至少成功插入一部分元素")
		})

		t.Run("驱逐路径中插入成功的元素保持可命中", func(t *testing.T) {
			// 用低负载验证插入成功元素的可命中性（无驱逐干扰）
			fresh := rdb.NewCuckooFilter("cfh:3", redis.WithCuckooCapacity(200))
			require.NoError(t, rdb.Del(ctx, "cfh:3").Err())

			for i := range 50 {
				added, err := fresh.Add(ctx, fmt.Sprintf("keep-%d", i))
				require.NoError(t, err)
				require.True(t, added, "低负载下插入应成功")
			}
			for i := range 50 {
				exists, err := fresh.Exists(ctx, fmt.Sprintf("keep-%d", i))
				require.NoError(t, err)
				assert.True(t, exists, "低负载下插入的元素应全部可命中")
			}
		})

		t.Run("Info 占用统计", func(t *testing.T) {
			info, err := cf.Info(ctx)
			require.NoError(t, err)
			assert.NotNil(t, info)
			assert.GreaterOrEqual(t, info.NumItems, int64(0), "NumItems 应为非负")
			t.Logf("Info: buckets=%d items=%d size=%d bucketSize=%d",
				info.NumBuckets, info.NumItems, info.Size, info.BucketSize)

			// NumItems 应与桶内指纹总数一致（本测试中 cfh:1 内 item1 已删除，
			// 无其他残留，故为 0；若前面子测试顺序变化此处不强制精确值）
			_ = info
		})

		t.Run("默认参数（无 Option）", func(t *testing.T) {
			def := rdb.NewCuckooFilter("cfh:4")
			require.NoError(t, rdb.Del(ctx, "cfh:4").Err())

			added, err := def.Add(ctx, "d1")
			require.NoError(t, err)
			assert.True(t, added)

			exists, err := def.Exists(ctx, "d1")
			require.NoError(t, err)
			assert.True(t, exists)

			info, err := def.Info(ctx)
			require.NoError(t, err)
			assert.Equal(t, int64(4), info.BucketSize, "默认桶大小应为 4")
			assert.Equal(t, int64(1), info.NumItems)
		})
	})
}

// TestCuckooHashReset 验证回退版 Reset 基本语义：灌入若干元素后整键销毁
// （EXISTS==0、Info 占用统计归零、原 item Exists false、Del false），
// 且 Reset 后可继续正常使用（Add 重新建桶）。
func TestCuckooHashReset(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		ctx := context.Background()
		key := "cfh:reset"
		require.NoError(t, rdb.Del(ctx, key).Err())

		cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(1000))
		for i := range 5 {
			added, err := cf.Add(ctx, fmt.Sprintf("r-%d", i))
			require.NoError(t, err)
			require.True(t, added)
		}

		info, err := cf.Info(ctx)
		require.NoError(t, err)
		require.Greater(t, info.NumItems, int64(0), "Reset 前应有占用")

		require.NoError(t, cf.Reset(ctx))

		// 物理键已删除
		exists, err := rdb.Exists(ctx, key).Result()
		require.NoError(t, err)
		assert.Equal(t, int64(0), exists, "Reset 后物理键应不存在")

		// 占用统计归零（NumItems/NumBuckets；BucketSize 是配置字段不参与归零）
		info, err = cf.Info(ctx)
		require.NoError(t, err)
		assert.Zero(t, info.NumItems, "Reset 后 NumItems 应为 0")
		assert.Zero(t, info.NumBuckets, "Reset 后 NumBuckets 应为 0")

		// 同 item 查不到、删不掉
		hit, err := cf.Exists(ctx, "r-0")
		require.NoError(t, err)
		assert.False(t, hit, "Reset 后原 item 不应命中")
		deleted, err := cf.Del(ctx, "r-0")
		require.NoError(t, err)
		assert.False(t, deleted, "Reset 后原 item 删除应返回 false")

		// Reset 后过滤器可继续使用
		added, err := cf.Add(ctx, "after-reset")
		require.NoError(t, err)
		assert.True(t, added, "Reset 后应可继续添加")
		hit, err = cf.Exists(ctx, "after-reset")
		require.NoError(t, err)
		assert.True(t, hit)
	})
}

// TestCuckooHashResetIdempotent 验证幂等：从未存在的键 Reset 无错
// （DEL 对不存在键返回 0 而非错误），可安全重复调用/失败重试。
func TestCuckooHashResetIdempotent(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		ctx := context.Background()

		cf := rdb.NewCuckooFilter("cfh:reset-idem")
		require.NoError(t, rdb.Del(ctx, "cfh:reset-idem").Err())

		// 键不存在时 Reset 无错
		require.NoError(t, cf.Reset(ctx))

		// 连发两次 Reset 无错
		_, err := cf.Add(ctx, "x")
		require.NoError(t, err)
		require.NoError(t, cf.Reset(ctx))
		require.NoError(t, cf.Reset(ctx))
	})
}

// TestCuckooHashResetFailure 验证失败语义：服务不可用时 Reset 恒返回错误，
// FailOpen 也不放行（"没清掉却假装清了"会让会话隔离静默失效）；错误经
// fallbackErr 包装，errors.Is(ErrRedisUnavailable) 可感知。
// 复用 failover_test.go 的 newFailedClient（同包测试辅助）。
func TestCuckooHashResetFailure(t *testing.T) {
	ctx := context.Background()
	rdb := newFailedClient(t)

	// 默认 FailOpen：仍返回错误
	cf := rdb.NewCuckooFilter("cfh:reset-fail")
	require.ErrorIs(t, cf.Reset(ctx), redis.ErrRedisUnavailable,
		"FailOpen 下 Reset 失败也必须返回错误（不走兜底放行）")

	// 显式 FailClosed：同样返回错误
	cfClosed := rdb.NewCuckooFilter("cfh:reset-fail2",
		redis.WithFailPolicy[*redis.CuckooConfig](redis.FailClosed))
	require.ErrorIs(t, cfClosed.Reset(ctx), redis.ErrRedisUnavailable)
}

// TestCuckooHashResetConcurrent 并发冒烟（-race 下跑）：Reset×Add×Exists
// 混跑，验证无 panic、无数据竞争。Reset 与并发 Add 无相对次序承诺，
// 故只容忍错误、不断言结果。
func TestCuckooHashResetConcurrent(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		ctx := context.Background()
		key := "cfh:reset-race"
		require.NoError(t, rdb.Del(ctx, key).Err())

		cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(500))

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range 50 {
			wg.Go(func() {
				<-start
				_, _ = cf.Add(ctx, fmt.Sprintf("race-%d", i))
			})
			wg.Go(func() {
				<-start
				_, _ = cf.Exists(ctx, fmt.Sprintf("race-%d", i))
			})
		}
		for range 10 {
			wg.Go(func() {
				<-start
				_ = cf.Reset(ctx)
			})
		}
		close(start)
		wg.Wait()

		// 收尾：最终一次 Reset 后键必须可查（无 panic、状态一致即达冒烟目的）
		require.NoError(t, cf.Reset(ctx))
		exists, err := rdb.Exists(ctx, key).Result()
		require.NoError(t, err)
		assert.Equal(t, int64(0), exists)
	})
}

// TestCuckooHashExistsMulti 验证回退版批量存在性检查：结果与逐条 Exists
// 一致性锚定（逐一相等）、顺序对应、空入参 (nil, nil)、不支持类型混入时
// 整体数据类错误且不发命令（键内容不变）。
func TestCuckooHashExistsMulti(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		ctx := context.Background()
		key := "cfh:mexists"
		require.NoError(t, rdb.Del(ctx, key).Err())

		cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(1000))
		for _, it := range []string{"m-0", "m-1", "m-2"} {
			_, err := cf.Add(ctx, it)
			require.NoError(t, err)
		}

		queries := []any{"m-0", "ghost-a", "m-1", "ghost-b", "m-2", "m-0"}
		got, err := cf.ExistsMulti(ctx, queries...)
		require.NoError(t, err)
		require.Len(t, got, len(queries), "结果必须与入参一一对应")

		// 一致性锚定：与逐条 Exists 全等（含假阳性场景也无分叉）
		for i, q := range queries {
			want, err := cf.Exists(ctx, q)
			require.NoError(t, err)
			assert.Equal(t, want, got[i], "ExistsMulti 第 %d 项与单条 Exists 分叉", i)
		}
		// 已添加项必命中（无假阴性）
		assert.True(t, got[0] && got[2] && got[4])

		// 空入参（对齐 bloom 惯例）
		res, err := cf.ExistsMulti(ctx)
		require.NoError(t, err)
		assert.Nil(t, res, "空入参应返回 (nil, nil)")

		// 不支持类型混入 → 整体数据类错误、不发命令（键内容不变）
		before, err := rdb.HGetAll(ctx, key).Result()
		require.NoError(t, err)
		res, err = cf.ExistsMulti(ctx, "m-0", struct{ X int }{1})
		require.Error(t, err)
		assert.Nil(t, res)
		assert.False(t, redis.IsUnavailable(err), "编码错误必须数据类原样返回，got %v", err)
		after, err := rdb.HGetAll(ctx, key).Result()
		require.NoError(t, err)
		assert.Equal(t, before, after, "整体报错时不得发命令（键内容不变）")
	})
}

// TestCuckooHashCount 验证回退版 Count：未添加→0（无假阴性）、添加后→1
// （去重语义、重复 Add 不增计数）、Del 后归 0。
func TestCuckooHashCount(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		ctx := context.Background()
		key := "cfh:count"
		require.NoError(t, rdb.Del(ctx, key).Err())

		cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(1000))

		n, err := cf.Count(ctx, "c-1")
		require.NoError(t, err)
		assert.Equal(t, int64(0), n, "未添加元素 Count 必须为 0（无假阴性）")

		_, err = cf.Add(ctx, "c-1")
		require.NoError(t, err)
		n, err = cf.Count(ctx, "c-1")
		require.NoError(t, err)
		assert.Equal(t, int64(1), n, "回退版去重语义：添加后 Count 恒 1")

		// 重复 Add 不增加计数（去重式，与模块版多重集不同）
		_, err = cf.Add(ctx, "c-1")
		require.NoError(t, err)
		n, err = cf.Count(ctx, "c-1")
		require.NoError(t, err)
		assert.Equal(t, int64(1), n, "回退版重复 Add 不增计数")

		deleted, err := cf.Del(ctx, "c-1")
		require.NoError(t, err)
		require.True(t, deleted)
		n, err = cf.Count(ctx, "c-1")
		require.NoError(t, err)
		assert.Equal(t, int64(0), n, "Del 后 Count 归 0")
	})
}

// TestCuckooHashAddNX 验证回退版 AddNX：首次 true、重复 false（NX 语义），
// 以及回退版 Add 与 AddNX 的等价性锚定——两个同构过滤器分别只走 Add /
// 只走 AddNX，同一序列逐项返回值恒相同。
func TestCuckooHashAddNX(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		ctx := context.Background()
		kAdd, kNX := "cfh:nx-add", "cfh:nx-only"
		require.NoError(t, rdb.Del(ctx, kAdd, kNX).Err())

		cfNX := rdb.NewCuckooFilter(kNX, redis.WithCuckooCapacity(1000))
		added, err := cfNX.AddNX(ctx, "n-1")
		require.NoError(t, err)
		assert.True(t, added, "首次插入应成功")

		added, err = cfNX.AddNX(ctx, "n-1")
		require.NoError(t, err)
		assert.False(t, added, "已存在元素 NX 应不插入")

		// 等价性锚定：回退版 Add 本就是 NX（cuckooAddScript 存在即返回 0），
		// 两方法对同一序列恒同。
		cfAdd := rdb.NewCuckooFilter(kAdd, redis.WithCuckooCapacity(1000))
		seq := []string{"k-1", "k-2", "k-1", "k-3", "k-2", "k-1", "k-4"}
		for _, s := range seq {
			a, err := cfAdd.Add(ctx, s)
			require.NoError(t, err)
			x, err := cfNX.AddNX(ctx, s)
			require.NoError(t, err)
			assert.Equal(t, a, x, "回退版 Add 与 AddNX 对同一序列项 %q 返回值分叉", s)
		}
	})
}

// TestCuckooFilterMultiOpsFailover 验证新方法的服务失效兜底分叉：
// ExistsMulti 整体兜底（FailOpen 全 true / FailClosed 全 false）+ 哨兵，
// 禁止混合结果；Count 观测类恒 (0, 哨兵)、不随策略分叉；AddNX 写布尔类
// 与 Add 同走 fallbackBool。复用 failover_test.go 的 newFailedClient。
func TestCuckooFilterMultiOpsFailover(t *testing.T) {
	ctx := context.Background()
	rdb := newFailedClient(t)

	cf := rdb.NewCuckooFilter("cfh:fail-multi") // 默认 FailOpen
	cfClosed := rdb.NewCuckooFilter("cfh:fail-multi2",
		redis.WithFailPolicy[*redis.CuckooConfig](redis.FailClosed))

	t.Run("ExistsMulti 整体兜底", func(t *testing.T) {
		res, err := cf.ExistsMulti(ctx, "a", "b", "c")
		require.ErrorIs(t, err, redis.ErrRedisUnavailable)
		require.Len(t, res, 3, "兜底切片长度必须与入参一致")
		for i, v := range res {
			assert.True(t, v, "FailOpen 下第 %d 项应为 true（禁止混合结果）", i)
		}

		res, err = cfClosed.ExistsMulti(ctx, "a", "b")
		require.ErrorIs(t, err, redis.ErrRedisUnavailable)
		require.Len(t, res, 2)
		for i, v := range res {
			assert.False(t, v, "FailClosed 下第 %d 项应为 false", i)
		}
	})

	t.Run("Count 观测类零值兜底", func(t *testing.T) {
		// FailOpen 不得返回"放行"语义的非零值——计数只能诚实为 0
		n, err := cf.Count(ctx, "a")
		require.ErrorIs(t, err, redis.ErrRedisUnavailable)
		assert.Equal(t, int64(0), n, "观测类兜底恒零值，不随策略分叉")

		n, err = cfClosed.Count(ctx, "a")
		require.ErrorIs(t, err, redis.ErrRedisUnavailable)
		assert.Equal(t, int64(0), n)
	})

	t.Run("AddNX 写布尔类兜底", func(t *testing.T) {
		added, err := cf.AddNX(ctx, "a")
		require.ErrorIs(t, err, redis.ErrRedisUnavailable)
		assert.True(t, added, "FailOpen 按策略视为已独占")

		added, err = cfClosed.AddNX(ctx, "a")
		require.ErrorIs(t, err, redis.ErrRedisUnavailable)
		assert.False(t, added, "FailClosed 应拒绝")
	})
}

// TestCuckooHashMultiOpsConcurrent 并发冒烟（-race 下跑）：ExistsMulti×
// Count×AddNX×Reset 混跑，验证新方法无 panic、无数据竞争。结果不做断言
// （并发 Reset 世代效应）。
func TestCuckooHashMultiOpsConcurrent(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		ctx := context.Background()
		key := "cfh:multi-race"
		require.NoError(t, rdb.Del(ctx, key).Err())

		cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(500))

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range 40 {
			wg.Go(func() {
				<-start
				_, _ = cf.AddNX(ctx, fmt.Sprintf("mr-%d", i))
			})
			wg.Go(func() {
				<-start
				_, _ = cf.ExistsMulti(ctx, fmt.Sprintf("mr-%d", i), fmt.Sprintf("mx-%d", i))
			})
			wg.Go(func() {
				<-start
				_, _ = cf.Count(ctx, fmt.Sprintf("mr-%d", i))
			})
		}
		for range 8 {
			wg.Go(func() {
				<-start
				_ = cf.Reset(ctx)
			})
		}
		close(start)
		wg.Wait()

		require.NoError(t, cf.Reset(ctx))
		exists, err := rdb.Exists(ctx, key).Result()
		require.NoError(t, err)
		assert.Equal(t, int64(0), exists)
	})
}

// TestCuckooHashAddMultiConsistency 关键锚定用例：批量 AddMulti 的结果与
// 逐条 Add **逐一相等**（防 cuckooAddMultiScript 与 cuckooAddScript 逻辑
// 漂移的回归防线，两脚本同源声明见 cuckoo.go 脚本注释）。序列刻意混入
// 重复项（触发已存在检查路径）与足量元素（触发部分驱逐路径）；除逐项
// 结果相等外，额外断言两键最终 Hash 内容全等（状态等价锚定，捕获结果
// 数组相同但桶布局漂移的场景）。同时覆盖空入参 (nil, nil) 与不支持类型
// 混入 → 整体数据类错误且不发命令（键内容不变）。
func TestCuckooHashAddMultiConsistency(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		ctx := context.Background()
		kSeq, kBatch := "cfh:am-seq", "cfh:am-batch"
		require.NoError(t, rdb.Del(ctx, kSeq, kBatch).Err())

		// 同构配置：capacity/bucketSize 一致 → fp/i1/i2 序列与驱逐决策一致
		opt := redis.WithCuckooCapacity(1000)
		cfSeq := rdb.NewCuckooFilter(kSeq, opt)
		cfBatch := rdb.NewCuckooFilter(kBatch, opt)

		// 构造含重复的确定性序列（dup 项触发 0 结果；200 项保持低负载，
		// 避免超载驱逐使结果不确定化——两路径若都超载仍应相等，但锚定
		// 意图放在纯逻辑漂移上）
		var items []any
		for i := range 100 {
			items = append(items, fmt.Sprintf("am-%d", i))
		}
		for i := range 30 {
			items = append(items, fmt.Sprintf("am-%d", i%20)) // 重复项
		}

		// 逐条 Add
		seqResults := make([]bool, len(items))
		for i, it := range items {
			added, err := cfSeq.Add(ctx, it)
			require.NoError(t, err, "逐条 Add 第 %d 项", i)
			seqResults[i] = added
		}

		// 批量 AddMulti（单条 EVAL）
		batchResults, err := cfBatch.AddMulti(ctx, items...)
		require.NoError(t, err)
		require.Len(t, batchResults, len(items), "结果必须与入参一一对应")

		for i := range items {
			assert.Equal(t, seqResults[i], batchResults[i],
				"AddMulti 第 %d 项（%v）与逐条 Add 分叉（脚本体漂移？）", i, items[i])
		}

		// 状态等价锚定：两键最终 Hash 内容全等（field 与 value 逐一相同）
		hSeq, err := rdb.HGetAll(ctx, kSeq).Result()
		require.NoError(t, err)
		hBatch, err := rdb.HGetAll(ctx, kBatch).Result()
		require.NoError(t, err)
		assert.Equal(t, hSeq, hBatch, "批量与逐条插入后的桶布局不一致（驱逐链决策漂移）")

		// 空入参（对齐门面 ExistsMulti 惯例）
		res, err := cfBatch.AddMulti(ctx)
		require.NoError(t, err)
		assert.Nil(t, res, "空入参应返回 (nil, nil)")

		// 不支持类型混入 → 整体数据类错误、不发命令（键内容不变）
		before, err := rdb.HGetAll(ctx, kBatch).Result()
		require.NoError(t, err)
		res, err = cfBatch.AddMulti(ctx, "ok-item", struct{ X int }{1})
		require.Error(t, err)
		assert.Nil(t, res)
		assert.False(t, redis.IsUnavailable(err), "编码错误必须数据类原样返回，got %v", err)
		after, err := rdb.HGetAll(ctx, kBatch).Result()
		require.NoError(t, err)
		assert.Equal(t, before, after, "整体报错时不得发命令（键内容不变）")
	})
}

// TestCuckooHashAddMultiSmoke1000 大批量冒烟：N=1000 单条 EVAL 完成且
// 无错，记录实测耗时（成本声明"由调用方控批"的量化依据；miniredis 内存
// 操作，真机 Redis 数量级相当、Lua 执行为主项）。
func TestCuckooHashAddMultiSmoke1000(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		ctx := context.Background()
		key := "cfh:am-1000"
		require.NoError(t, rdb.Del(ctx, key).Err())

		// capacity 5000 → 1250 桶 × 4 槽，1000 项约 20% 负载，无超载拒绝
		cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(5000))

		items := make([]any, 1000)
		for i := range items {
			items[i] = fmt.Sprintf("smoke-%d", i)
		}

		start := time.Now()
		res, err := cf.AddMulti(ctx, items...)
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.Len(t, res, 1000)
		ok := 0
		for _, v := range res {
			if v {
				ok++
			}
		}
		t.Logf("AddMulti N=1000 实测耗时：%v（成功插入 %d/1000）", elapsed, ok)
		assert.Equal(t, 1000, ok, "低负载下 1000 项应全部插入成功")

		// 抽查存在性（无假阴性）
		hit, err := cf.Exists(ctx, "smoke-999")
		require.NoError(t, err)
		assert.True(t, hit)
	})
}

// TestCuckooFilterAddMultiFailover 验证 AddMulti 的服务失效兜底：整体
// fallbackBools（FailOpen 全 true / FailClosed 全 false）+ 哨兵错误可感知，
// 禁止混合结果。复用 failover_test.go 的 newFailedClient。
func TestCuckooFilterAddMultiFailover(t *testing.T) {
	ctx := context.Background()
	rdb := newFailedClient(t)

	cf := rdb.NewCuckooFilter("cfh:am-fail") // 默认 FailOpen
	res, err := cf.AddMulti(ctx, "a", "b", "c")
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	require.Len(t, res, 3, "兜底切片长度必须与入参一致")
	for i, v := range res {
		assert.True(t, v, "FailOpen 下第 %d 项应为 true（禁止混合结果）", i)
	}

	cfClosed := rdb.NewCuckooFilter("cfh:am-fail2",
		redis.WithFailPolicy[*redis.CuckooConfig](redis.FailClosed))
	res, err = cfClosed.AddMulti(ctx, "a", "b")
	require.ErrorIs(t, err, redis.ErrRedisUnavailable)
	require.Len(t, res, 2)
	for i, v := range res {
		assert.False(t, v, "FailClosed 下第 %d 项应为 false", i)
	}
}

// TestCuckooHashAddMultiConcurrent 并发冒烟（-race 下跑）：AddMulti×
// ExistsMulti×Reset 混跑，验证新方法组合无 panic、无数据竞争。结果不做
// 断言（并发 Reset 世代效应）。
func TestCuckooHashAddMultiConcurrent(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		ctx := context.Background()
		key := "cfh:am-race"
		require.NoError(t, rdb.Del(ctx, key).Err())

		cf := rdb.NewCuckooFilter(key, redis.WithCuckooCapacity(500))

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range 30 {
			wg.Go(func() {
				<-start
				_, _ = cf.AddMulti(ctx,
					fmt.Sprintf("ar-%d", i), fmt.Sprintf("ar-%d", i), fmt.Sprintf("ax-%d", i))
			})
			wg.Go(func() {
				<-start
				_, _ = cf.ExistsMulti(ctx, fmt.Sprintf("ar-%d", i), fmt.Sprintf("ax-%d", i))
			})
		}
		for range 8 {
			wg.Go(func() {
				<-start
				_ = cf.Reset(ctx)
			})
		}
		close(start)
		wg.Wait()

		require.NoError(t, cf.Reset(ctx))
		exists, err := rdb.Exists(ctx, key).Result()
		require.NoError(t, err)
		assert.Equal(t, int64(0), exists)
	})
}
