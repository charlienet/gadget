package redis

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// v0.11.0 G4：NewBloomFilter 的模块期望声明（WithModule）与路径自检
// 观测（BloomInfo.Path）。背景事故：Capability 未探测时工厂静默分派
// bitmap 路径，对既有 MBbloom-- 键执行 STRLEN/GETBIT 报 WRONGTYPE。

// setProbedForTest 白盒直设 Capability 探测缓存（构造「已探测但无模块」
// 等离线不可达状态；持锁直设，稳定可重复不留 flaky）。
func setProbedForTest(t *testing.T, c *Capability, probed bool, modules []moduleInfo, hasCuckoo bool) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probed = probed
	c.modules = modules
	c.hasCuckoo = hasCuckoo
}

// TestBloomModuleBFNotProbed BF + 未探测 → ErrCapabilityNotProbed，且
// 校验发生在 connectAll 之前：零命令副作用（不发 RESERVE/不建键）。
func TestBloomModuleBFNotProbed(t *testing.T) {
	ctx := t.Context()
	rc, mr := newMiniRedisClient(t)

	baseline := mr.CommandCount()
	_, err := rc.NewBloomFilter(ctx, "bm:notprobed", WithModule(BloomModuleBF))
	if !errors.Is(err, ErrCapabilityNotProbed) {
		t.Fatalf("BF+未探测应报 ErrCapabilityNotProbed，got %v", err)
	}
	if got := mr.CommandCount(); got != baseline {
		t.Fatalf("校验失败须零命令副作用（connectAll 之前）：CommandCount %d → %d", baseline, got)
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("校验失败不得建键，got %v", keys)
	}
}

// TestBloomModuleBFProbedNoModule BF + 已探测但无 bf 模块 →
// ErrModuleNotLoaded，错误串携带实际探测判定（HasBloom）。
// 「已探测无模块」用白盒直设（miniredis 无 INFO section 支持，真实
// 探测必失败，无法离线达成该态）。
func TestBloomModuleBFProbedNoModule(t *testing.T) {
	ctx := t.Context()
	rc, mr := newMiniRedisClient(t)

	setProbedForTest(t, rc.cap, true, []moduleInfo{{Name: "ReJSON"}}, false)

	baseline := mr.CommandCount()
	_, err := rc.NewBloomFilter(ctx, "bm:nobf", WithModule(BloomModuleBF))
	if !errors.Is(err, ErrModuleNotLoaded) {
		t.Fatalf("BF+探测无模块应报 ErrModuleNotLoaded，got %v", err)
	}
	if !strings.Contains(err.Error(), "HasBloom") {
		t.Fatalf("错误串应携带实际探测判定（HasBloom），got %v", err)
	}
	if got := mr.CommandCount(); got != baseline {
		t.Fatalf("报错同样须零命令副作用：CommandCount %d → %d", baseline, got)
	}
}

// TestBloomModuleBitmapForcesBitmap Bitmap + 已探测有 bf → 无条件强制
// bitmap 路径、成功构造（评审裁决：不查探测状态、不报错）。miniredis
// 不支持 BF.*，bf 路径构造必失败——构造成功本身即强制通道的行为判据，
// 另加类型断言与 Info().Path 双重钉死。
func TestBloomModuleBitmapForcesBitmap(t *testing.T) {
	ctx := t.Context()
	rc, _ := newMiniRedisClient(t)

	// 探测态：probed=true 且 bf 在场（若实现误查 HasBloom 走 bf 路径，
	// miniredis 上 BF.RESERVE 报 unknown command → 构造失败，用例即红）。
	setProbedForTest(t, rc.cap, true, []moduleInfo{{Name: "bf"}}, false)

	f, err := rc.NewBloomFilter(ctx, "bm:forcebit",
		WithCapacity(1000), WithFalsePositive(0.01), WithModule(BloomModuleBitmap))
	if err != nil {
		t.Fatalf("Bitmap 强制通道不得因探测态报错：%v", err)
	}
	if _, ok := f.(*bitmapImpl); !ok {
		t.Fatalf("应交付 bitmapImpl，got %T", f)
	}
	info, err := f.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path != PathBitmap {
		t.Fatalf("bitmap 路径 Info().Path 应为 %q，got %q", PathBitmap, info.Path)
	}

	// 未探测状态同样不报错（Bitmap 值不查探测状态）。
	rc2, mr2 := newMiniRedisClient(t)
	_ = mr2
	if _, err := rc2.NewBloomFilter(ctx, "bm:forcebit2",
		WithCapacity(1000), WithFalsePositive(0.01), WithModule(BloomModuleBitmap)); err != nil {
		t.Fatalf("Bitmap+未探测应无条件成功：%v", err)
	}
}

// TestBloomModuleAutoRegression Auto（零值，含不带 Option）与 v0.10.0
// 逐行为一致：探测态 bf 在场走 bf（离线不可达，真机 gate 覆盖于
// TestBloomModuleBFPathRealRedis）、缺席/未探测走 bitmap 且不报错。
func TestBloomModuleAutoRegression(t *testing.T) {
	ctx := t.Context()

	t.Run("未探测走bitmap不报错", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		f, err := rc.NewBloomFilter(ctx, "bm:auto-off", WithCapacity(1000), WithFalsePositive(0.01))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := f.(*bitmapImpl); !ok {
			t.Fatalf("未探测保守态应走 bitmap，got %T", f)
		}
	})

	t.Run("已探测无bf走bitmap不报错", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		setProbedForTest(t, rc.cap, true, nil, false)
		f, err := rc.NewBloomFilter(ctx, "bm:auto-no",
			WithCapacity(1000), WithFalsePositive(0.01), WithModule(BloomModuleAuto))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := f.(*bitmapImpl); !ok {
			t.Fatalf("显式 Auto 应与零值行为一致走 bitmap，got %T", f)
		}
	})
}

// TestBloomModulePathOffline bitmap 路径 Info().Path（离线）+ prefill
// 装饰器 Info 透传 Path（bloom_prefill_filter.go 零改动确认）。
func TestBloomModulePathOffline(t *testing.T) {
	ctx := t.Context()
	rc, _ := newMiniRedisClient(t)

	f, err := rc.NewBloomFilter(ctx, "bm:path", WithCapacity(1000), WithFalsePositive(0.01))
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path != PathBitmap {
		t.Fatalf("Info().Path 应为 %q，got %q", PathBitmap, info.Path)
	}

	// prefill 装饰器：Info 恒透传 inner，Path 随之透传。
	fp, err := rc.NewBloomFilter(ctx, "bm:path-pf",
		WithCapacity(1000), WithFalsePositive(0.01), WithModule(BloomModuleBitmap),
		WithPrefill(func(context.Context, PrefillIngest) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fp.(*prefillFilter); !ok {
		t.Fatalf("启用 WithPrefill 应返回 *prefillFilter，got %T", fp)
	}
	pinfo, err := fp.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pinfo.Path != PathBitmap {
		t.Fatalf("prefill 装饰器应透传 Info().Path=%q，got %q", PathBitmap, pinfo.Path)
	}
}

// TestBloomModuleBFPathRealRedis 真 Redis + bf 模块（gate：REDIS_URL 未
// 设置或无 bf 模块即 Skip，对齐 prefill_concurrency_integration_test.go
// 既有口径）：BF 显式声明构造成功走 BF.* 路径，Info().Path==PathBF；
// Auto 探测有 bf 时同样分派 bfCmdImpl（v0.10.0 行为回归）。
func TestBloomModuleBFPathRealRedis(t *testing.T) {
	ctx := t.Context()
	rc := newRealStandaloneClient(t)

	if err := rc.Capability().Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !rc.Capability().HasBloom() {
		t.Skip("server has no bf module; skip BF path real-Redis assertions")
	}

	key := bloomTestKey("bf-path")
	t.Cleanup(func() { _ = rc.Del(ctx, key).Err() })

	f, err := rc.NewBloomFilter(ctx, key,
		WithCapacity(1000), WithFalsePositive(0.01), WithModule(BloomModuleBF))
	if err != nil {
		t.Fatalf("BF+已探测有模块应成功构造：%v", err)
	}
	if _, ok := f.(*bfCmdImpl); !ok {
		t.Fatalf("BF 声明应交付 bfCmdImpl，got %T", f)
	}
	info, err := f.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path != PathBF {
		t.Fatalf("BF 路径 Info().Path 应为 %q，got %q", PathBF, info.Path)
	}

	// Auto 回归：bf 在场走 bf 路径（与 v0.10.0 一致）。
	key2 := bloomTestKey("auto-bf")
	t.Cleanup(func() { _ = rc.Del(ctx, key2).Err() })
	f2, err := rc.NewBloomFilter(ctx, key2, WithCapacity(1000), WithFalsePositive(0.01))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f2.(*bfCmdImpl); !ok {
		t.Fatalf("Auto+已探测有 bf 应走 bfCmdImpl，got %T", f2)
	}
}

// TestBloomModuleConcurrent -race 并发口径：并发以不同模块期望调工厂
// 与并发读探测状态，无数据竞争、Bitmap 通道恒成功。
func TestBloomModuleConcurrent(t *testing.T) {
	ctx := t.Context()
	rc, mr := newMiniRedisClient(t)

	setProbedForTest(t, rc.cap, true, []moduleInfo{{Name: "bf"}}, false)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 2 {
			case 0:
				key := "bm:conc-bitmap-" + string(rune('a'+i))
				if _, err := rc.NewBloomFilter(ctx, key,
					WithCapacity(1000), WithFalsePositive(0.01), WithModule(BloomModuleBitmap)); err != nil {
					t.Errorf("Bitmap 强制通道并发构造失败: %v", err)
				}
			case 1:
				_ = rc.cap.Probed()
				_ = rc.cap.HasBloom()
			}
		}(i)
	}
	wg.Wait()
	_ = mr
}
