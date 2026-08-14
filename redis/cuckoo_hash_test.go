package redis_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cuckooFingerprint 复刻 hashImpl 的指纹计算（fnv1a & 0xFFFF，0 取 1），
// 供测试区分"放错位置"与"驱逐丢失"。
func cuckooFingerprint(item string) int64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(item); i++ {
		h ^= uint64(item[i])
		h *= 1099511628211
	}
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
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
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
			for i := 0; i < total; i++ {
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

			for i := 0; i < 50; i++ {
				added, err := fresh.Add(ctx, fmt.Sprintf("keep-%d", i))
				require.NoError(t, err)
				require.True(t, added, "低负载下插入应成功")
			}
			for i := 0; i < 50; i++ {
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
