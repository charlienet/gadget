package redis

// 集群双路径对照（自 cluster_integration_test.go 的"布隆 WithImpl 强制
// 路径对照"子测试迁移）：强制实现路径 Option 删除后，对照语义改由
// **白盒直构**保证——bf 路径 rc.newBFImpl、回退路径 newBitmapImpl，
// 不经工厂分派，与服务器是否加载 bf 模块无关（模块前提仍由 HasBloom
// 守卫，BF 侧命令可执行是断言成立的前提）。
// 断言强度与迁移前一致：双路径各 300 元素 AddMulti/ExistsMulti 自洽 +
// Info 合理性 + TYPE 探测（分片键存储结构互异）+ CLUSTER KEYSLOT 一致性
// + 裸 base 键不存在。
// 环境守卫：REDIS_CLUSTER 未设置或集群无 bf 模块时 skip（internal 不能
// import redis/test 包——循环依赖，自建 Getenv 守卫）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// randHex 生成 n 位随机 hex 段（键隔离用）。external 包 redis_test.go 的
// randomHex 跨包不可用，internal 测试自带等价实现。
func randHex(n int) string {
	b := make([]byte, n/2+1)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)[:n]
}

// TestBloomABPathsShardedCluster 真实集群、同参数（capacity=100000、显式
// WithShardCount(8) → effectiveN=8）下 BF.* 与 bitmap 两条路径的分片行为
// 对照（直构保证路径归属）。
func TestBloomABPathsShardedCluster(t *testing.T) {
	raw := os.Getenv("REDIS_CLUSTER")
	if raw == "" {
		t.Skip("REDIS_CLUSTER 未设置：跳过集群双路径对照")
	}
	rdb, err := NewWithUrl(raw)
	if err != nil {
		t.Fatalf("NewWithUrl: %v", err)
	}
	defer func() { _ = rdb.GracefulClose(context.Background()) }()
	if !rdb.Capability().HasModule("bf") {
		t.Skip("集群未加载 bf 模块，跳过双路径对照（BF 侧前提不成立）")
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}
	if rc.Mode() != ModeCluster {
		t.Fatalf("连接模式应为 ModeCluster，got %v", rc.Mode())
	}

	ctx := context.Background()
	baseBF := fmt.Sprintf("bloomctest:%s", randHex(6))
	baseBMP := fmt.Sprintf("bloomctest:%s", randHex(6))
	allKeys := make([]string, 0, 16)
	for _, base := range []string{baseBF, baseBMP} {
		for i := range 8 {
			allKeys = append(allKeys, fmt.Sprintf("%s#%d", base, i))
		}
	}
	defer func() {
		for _, k := range allKeys { // 逐键 Del（多键跨 slot 会 CROSSSLOT）
			_ = rc.Del(ctx, k).Err()
		}
	}()

	cfgBF := defaultBloomConfig()
	WithCapacity(100_000)(&cfgBF)
	WithFalsePositive(0.01)(&cfgBF)
	WithShardCount(8)(&cfgBF)
	cfgBF.policy = FailOpen // 工厂原默认赋值，直构需显式补
	bf := rc.newBFImpl(baseBF, cfgBF)
	bmp := newBitmapImpl(rc, baseBMP, cfgBF)
	if !bf.sharder.enabled || bf.sharder.n != 8 {
		t.Fatalf("BF 分片未激活：enabled=%v n=%d", bf.sharder.enabled, bf.sharder.n)
	}
	if !bmp.sharder.enabled || bmp.sharder.n != 8 {
		t.Fatalf("bitmap 分片未激活：enabled=%v n=%d", bmp.sharder.enabled, bmp.sharder.n)
	}

	items := make([]any, 300)
	for i := range items {
		items[i] = fmt.Sprintf("abctest-%d", i)
	}

	// 分片态两路径均有预分配（BF 逐片惰性 BF.RESERVE；bitmap
	// m/k 按每分片容量），300 元素/8 片填充率 ~0.03%，"全新元素
	// AddMulti 全 true"的硬断言在双路径均成立。
	runPath := func(name string, f BloomFilter) *BloomInfo {
		added, err := f.AddMulti(ctx, items...)
		require.NoError(t, err, "%s 分片 AddMulti 失败（CROSSSLOT/路由类）", name)
		require.Len(t, added, len(items))
		for i, v := range added {
			require.True(t, v, "%s AddMulti[%d] 全新元素应判新增", name, i)
		}
		ex, err := f.ExistsMulti(ctx, items...)
		require.NoError(t, err, "%s 分片 ExistsMulti 失败", name)
		require.Len(t, ex, len(items))
		for i, v := range ex {
			require.True(t, v, "%s ExistsMulti[%d] 假阴性（回填错位或分片丢写）", name, i)
		}
		info, err := f.Info(ctx)
		require.NoError(t, err, "%s Info 聚合失败", name)
		assert.Positive(t, info.NumItems, "%s Info.NumItems", name)
		assert.Positive(t, info.Capacity, "%s Info.Capacity", name)
		assert.Positive(t, info.Size, "%s Info.Size", name)
		return info
	}
	infoBF := runPath("BF.*", bf)
	infoBMP := runPath("bitmap", bmp)
	// BF 路径聚合可见子过滤器计数；bitmap 无子过滤器概念（0）。
	// Capacity 不硬断言 ==100000：无 item 命中的分片不 RESERVE、
	// 贡献 0（空分片归一），随机散布下 Σ 为 100000 的整数分块。
	assert.GreaterOrEqual(t, infoBF.NumFilters, int64(1), "BF 路径 NumFilters 应 >=1")
	assert.Equal(t, int64(100_000), infoBMP.Capacity, "bitmap 路径 Capacity 上报配置总容量")

	// TYPE 探测：类型名随版本漂移（MBbloom--/batchloom/bf…），
	// 不硬编码字面量——断言 bitmap 侧为 string、BF 侧非 string，
	// 且两侧类型集合互不相交（证明两路径确实落在不同存储结构上）。
	typesOf := func(base string) map[string]bool {
		got := map[string]bool{}
		for i := range 8 {
			k := fmt.Sprintf("%s#%d", base, i)
			n, err := rc.Exists(ctx, k).Result()
			require.NoError(t, err, "EXISTS %s", k)
			if n == 0 {
				continue // 极端散布巧合下的空分片：不参与类型对照
			}
			tp, err := rc.Type(ctx, k).Result()
			require.NoError(t, err, "TYPE %s", k)
			got[tp] = true
		}
		return got
	}
	tBF, tBMP := typesOf(baseBF), typesOf(baseBMP)
	t.Logf("BF.* 分片键类型集合=%v，bitmap 分片键类型集合=%v", tBF, tBMP)
	require.NotEmpty(t, tBF, "BF 实例应有分片键")
	require.NotEmpty(t, tBMP, "bitmap 实例应有分片键")
	assert.NotContains(t, tBF, "string", "BF 路径分片键不应是 bitmap 字符串（路径选择未生效？）")
	assert.Contains(t, tBMP, "string", "bitmap 路径分片键应为 string")
	for tp := range tBF {
		assert.NotContains(t, tBMP, tp, "两路径分片键类型集合应互异，共享 %q", tp)
	}

	// CLUSTER KEYSLOT 收集某 base 的 8 个分片键 slot（服务端计算，
	// 客户端不引入 CRC16 实现）
	slotsOf := func(base string) []int64 {
		out := make([]int64, 0, 8)
		for i := range 8 {
			slot, err := rc.Do(ctx, "cluster", "keyslot",
				fmt.Sprintf("%s#%d", base, i)).Int64()
			require.NoError(t, err, "CLUSTER KEYSLOT %s#%d", base, i)
			out = append(out, slot)
		}
		return out
	}

	// slot 对照说明：规格要求"两实例同 base 分片键 slot 集合一致"
	// 无法按字面实现——两路径若共用同一物理键，先写方占住键类型，
	// 后写方必 WRONGTYPE（BF 与 bitmap 存储结构不同），因此两实例
	// 必须用不同 base。等价落实"路由与实现路径正交"：
	// ① 键的 slot 由服务端按键名唯一确定（同键两次 KEYSLOT 一致），
	//    与走哪条路径无关；② 两实例分片键各自散布多 slot。
	sBF1, sBF2 := slotsOf(baseBF), slotsOf(baseBF)
	assert.Equal(t, sBF1, sBF2, "同键 CLUSTER KEYSLOT 两次查询应一致（服务端确定性）")
	sBMP := slotsOf(baseBMP)
	distinctCount := func(sl []int64) int {
		m := map[int64]bool{}
		for _, s := range sl {
			m[s] = true
		}
		return len(m)
	}
	assert.Greater(t, distinctCount(sBF1), 1, "BF 分片键应散布多 slot，实际 %v", sBF1)
	assert.Greater(t, distinctCount(sBMP), 1, "bitmap 分片键应散布多 slot，实际 %v", sBMP)

	// 裸 base 键不存在（集群分片键名统一带 #idx 后缀）。
	// 逐键 EXISTS——多键跨 slot 会 CROSSSLOT。
	for _, base := range []string{baseBF, baseBMP} {
		n, err := rc.Exists(ctx, base).Result()
		require.NoError(t, err, "EXISTS 裸键 %s", base)
		assert.Equal(t, int64(0), n, "集群下不应存在无后缀的裸 base 键 %s", base)
	}
}
