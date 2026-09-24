package redis

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Phase() 本地相位访问器（v0.11.0 G2）与降级期错误可见性（G3）测试：
// 零 RTT 断言、Fresh 与 readyFresh 同口径、LastSyncErr 生命周期（记录/
// 清零/NotFound 口径）、未启用路径、storeLocal 多调用点不清零回归、
// 并发安全。白盒包内测试，构造时注入（newPrefillFilterForTest 已停
// loop），全部手动驱动 syncOnce，避免与后台 tick 竞争。

// --- 1) 零 RTT 纯内存读（对照 State 的 1 RTT） ---

func TestPhaseZeroRTT(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("phase-rtt")
	fn := func(context.Context, PrefillIngest) error { return nil }
	pf, _, mr := newPrefillFilterForTest(t, key, fn, WithSyncInterval(100*time.Millisecond))
	co := pf.coord

	// 预置权威 ready 并手动同步一拍：本地相位落快照（同时暖连接，
	// 排除后续 CommandCount 差值里的建连噪声）。
	if err := mr.Set(co.stateKey, pvReady); err != nil {
		t.Fatal(err)
	}
	co.syncOnce()

	n0 := mr.CommandCount()
	info, err := pf.Phase()
	if err != nil {
		t.Fatalf("Phase: %v", err)
	}
	if got := mr.CommandCount() - n0; got != 0 {
		t.Fatalf("Phase() 必须零 Redis 命令，实测多发 %d 条", got)
	}
	if info.Phase != PrefillReady || !info.Fresh || info.UpdatedAt.IsZero() {
		t.Fatalf("Phase 应 (Ready, Fresh, 非零时间): %+v", info)
	}

	// 对照：State(ctx) 强制 1 条 GET（权威读）。
	if _, err := pf.State(ctx); err != nil {
		t.Fatalf("State: %v", err)
	}
	if got := mr.CommandCount() - n0; got != 1 {
		t.Fatalf("State(ctx) 应恰发 1 条 GET，实测 %d 条", got)
	}
}

// --- 2) Fresh 与 readyFresh/isStale 同口径（阈值单源联动） ---

func TestPhaseFreshParity(t *testing.T) {
	fn := func(context.Context, PrefillIngest) error { return nil }
	pf, _, _ := newPrefillFilterForTest(t, bloomTestKey("phase-fresh"), fn, WithSyncInterval(100*time.Millisecond))
	co := pf.coord

	check := func(name string, wantFresh bool) {
		t.Helper()
		info, err := pf.Phase()
		if err != nil {
			t.Fatalf("%s Phase: %v", name, err)
		}
		if info.Fresh != wantFresh || co.readyFresh() != wantFresh {
			t.Fatalf("%s: Phase().Fresh=%v readyFresh=%v want=%v",
				name, info.Fresh, co.readyFresh(), wantFresh)
		}
	}

	// 新鲜 Ready：age≈0 < 2×100ms → Fresh=true
	co.storeLocal(PrefillReady)
	check("新鲜 Ready", true)

	// 超龄一侧：手动回拨 updatedAt 至 age=250ms（>200ms 阈值）→ 双双 false
	aged := &prefillLocal{phase: PrefillReady, updatedAt: time.Now().Add(-250 * time.Millisecond)}
	co.phase.Store(aged)
	check("超龄 Ready", false)
	if info, _ := pf.Phase(); !info.UpdatedAt.Equal(aged.updatedAt) {
		t.Fatalf("Phase().UpdatedAt 应为快照时间: got %v want %v", info.UpdatedAt, aged.updatedAt)
	}

	// 阈值联动（不运行期改共享 cfg——会与 loop goroutine 的读竞态；改用
	// 第二个大阈值实例承载同一超龄量）：syncInterval=300ms 时 2×300ms
	// =600ms > 250ms，同一超龄形态回到新鲜侧。两实例的 Fresh 都恒与各自
	// readyFresh 一致，证明出自同一 isStale 判定源而非复制阈值表达式。
	pf2, _, _ := newPrefillFilterForTest(t, bloomTestKey("phase-fresh2"), fn, WithSyncInterval(300*time.Millisecond))
	co2 := pf2.coord
	co2.phase.Store(&prefillLocal{phase: PrefillReady, updatedAt: time.Now().Add(-250 * time.Millisecond)})
	info2, err := pf2.Phase()
	if err != nil {
		t.Fatalf("大阈值实例 Phase: %v", err)
	}
	if !info2.Fresh || !co2.readyFresh() {
		t.Fatalf("大阈值（600ms 窗）下 age=250ms 应新鲜: Phase.Fresh=%v readyFresh=%v", info2.Fresh, co2.readyFresh())
	}

	// 非 Ready 一律 Fresh=false（即便新鲜）
	co.storeLocal(PrefillBuilding)
	check("新鲜 Building", false)
}

// --- 3) LastSyncErr 生命周期（记录/清零/NotFound 口径） ---

func TestPhaseLastSyncErrLifecycle(t *testing.T) {
	key := bloomTestKey("phase-err")
	fn := func(context.Context, PrefillIngest) error { return nil }
	pf, _, mr := newPrefillFilterForTest(t, key, fn, WithSyncInterval(50*time.Millisecond))
	co := pf.coord

	// 初始 nil 口径：状态键缺失 → GET redis.Nil（NotFound 是正常相位，
	// 非错误）：LastSyncErr=nil 且 LastSyncErrAt 保持零值。
	co.syncOnce()
	info, err := pf.Phase()
	if err != nil {
		t.Fatalf("Phase: %v", err)
	}
	if info.LastSyncErr != nil || !info.LastSyncErrAt.IsZero() {
		t.Fatalf("NotFound 拍应 (nil, 零时间): %+v", info)
	}
	// NotFound 触发过后台惰性重建（Uninitialized 短路放行），等其退出
	// 再注入故障，避免 finish 的 store 与断言竞争。
	waitInflightDone(co, 3*time.Second)

	// 注入非 NotFound 的 GET 故障：状态键改 hash 类型 → WRONGTYPE。
	mr.HSet(co.stateKey, "f", "v")
	co.syncOnce()
	info, _ = pf.Phase()
	if info.LastSyncErr == nil {
		t.Fatal("GET 失败拍应记录 LastSyncErr")
	}
	if IsNotFound(info.LastSyncErr) {
		t.Fatalf("注入的应为非 NotFound 错误，got %v", info.LastSyncErr)
	}
	if !strings.Contains(info.LastSyncErr.Error(), "WRONGTYPE") {
		t.Fatalf("错误值应原样存储不包装，got %v", info.LastSyncErr)
	}
	if info.LastSyncErrAt.IsZero() {
		t.Fatal("记录错误时 LastSyncErrAt 必须非零")
	}
	errAt := info.LastSyncErrAt

	// 恢复一拍成功：权威 ready → GET 成功 → err 清 nil、at 保留诊断。
	mr.Del(co.stateKey)
	if err := mr.Set(co.stateKey, pvReady); err != nil {
		t.Fatal(err)
	}
	co.syncOnce()
	info, _ = pf.Phase()
	if info.LastSyncErr != nil {
		t.Fatalf("成功拍应清 nil，got %v", info.LastSyncErr)
	}
	if !info.LastSyncErrAt.Equal(errAt) {
		t.Fatalf("清零后 LastSyncErrAt 应保留末次错误时间 %v，got %v", errAt, info.LastSyncErrAt)
	}

	// NotFound 拍（曾有错）：视为成功 → 清 nil、at 仍保留。
	mr.Del(co.stateKey)
	co.syncOnce()
	info, _ = pf.Phase()
	if info.LastSyncErr != nil || !info.LastSyncErrAt.Equal(errAt) {
		t.Fatalf("NotFound 拍应 (nil, at 保留): %+v", info)
	}
	waitInflightDone(co, 3*time.Second)
}

// --- 3b) Unavailable 错误类实测记录（mr.Close 宕机；评审 ISSUE-201） ---

// TestPhaseLastSyncErrUnavailable 补齐故障类别覆盖：Unavailable 类
// （连接类）错误同样必须记录进 LastSyncErr 且分类能力保留——Wrongtype
// 注入（用例 3）是数据类，不证明 IsUnavailable 路径。故障注入用
// mr.Close()（miniredis 不可复用、不复起，故独立成测试；库内先例见
// bloom_internal_test.go TestLuaSupportStateTransition、
// cuckoo_prefill_internal_test.go TestCuckooPrefillFailPolicy——
// Cleanup 重复 Close 幂等安全）。
func TestPhaseLastSyncErrUnavailable(t *testing.T) {
	key := bloomTestKey("phase-unavail")
	fn := func(context.Context, PrefillIngest) error { return nil }
	pf, _, mr := newPrefillFilterForTest(t, key, fn, WithSyncInterval(50*time.Millisecond))
	co := pf.coord

	// 成功一拍打底：确认打点前无误记（err nil），随后宕机才有对照。
	if err := mr.Set(co.stateKey, pvReady); err != nil {
		t.Fatal(err)
	}
	co.syncOnce()
	if info, _ := pf.Phase(); info.LastSyncErr != nil || !info.LastSyncErrAt.IsZero() {
		t.Fatalf("打底成功拍应 (nil, 零时间): %+v", info)
	}

	// 宕机：GET 走 dial 失败（连接类错误，非 NotFound）→ 必须记录。
	mr.Close()
	co.syncOnce()
	info, err := pf.Phase()
	if err != nil {
		t.Fatalf("Phase: %v", err)
	}
	if info.LastSyncErr == nil {
		t.Fatal("Unavailable 故障拍必须记录 LastSyncErr")
	}
	if !IsUnavailable(info.LastSyncErr) {
		t.Fatalf("LastSyncErr 应经 IsUnavailable 可分类（原样存储不包装），got %v", info.LastSyncErr)
	}
	if IsNotFound(info.LastSyncErr) {
		t.Fatalf("Unavailable 错误不得被误分类为 NotFound，got %v", info.LastSyncErr)
	}
	if info.LastSyncErrAt.IsZero() {
		t.Fatal("记录错误时 LastSyncErrAt 必须非零")
	}
	// 宕机后无命令可发：syncOnce 失败分支直接 return，不触后台重建，
	// 无需 waitInflightDone；cleanup 的重复 mr.Close 依先例幂等安全。
}

// --- 4) 未启用路径全部 ErrPrefillDisabled ---

func TestPhaseDisabledPaths(t *testing.T) {
	ctx := t.Context()

	t.Run("两裸 impl 空实现", func(t *testing.T) {
		for _, impl := range []BloomFilter{&bfCmdImpl{}, &bitmapImpl{}} {
			info, err := impl.Phase()
			if !errors.Is(err, ErrPrefillDisabled) || info != (PhaseInfo{}) {
				t.Fatalf("%T.Phase: got (%+v, %v)", impl, info, err)
			}
		}
	})

	t.Run("UnimplementedBloomFilter", func(t *testing.T) {
		var u UnimplementedBloomFilter
		if info, err := u.Phase(); !errors.Is(err, ErrPrefillDisabled) || info != (PhaseInfo{}) {
			t.Fatalf("Phase: got (%+v, %v)", info, err)
		}
	})

	t.Run("工厂未启用 bloom", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		f, err := rc.NewBloomFilter(ctx, bloomTestKey("phase-off"), WithCapacity(10_000), WithFalsePositive(0.0001))
		if err != nil {
			t.Fatalf("NewBloomFilter: %v", err)
		}
		if info, err := f.Phase(); !errors.Is(err, ErrPrefillDisabled) || info != (PhaseInfo{}) {
			t.Fatalf("未启用 Phase: got (%+v, %v)", info, err)
		}
	})

	t.Run("工厂未启用 cuckoo", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		cf, err := rc.NewCuckooFilter(ctx, cuckooTestKey("phase-off"), WithCuckooCapacity(10_000))
		if err != nil {
			t.Fatalf("NewCuckooFilter: %v", err)
		}
		if info, err := cf.Phase(); !errors.Is(err, ErrPrefillDisabled) || info != (PhaseInfo{}) {
			t.Fatalf("未启用 Phase: got (%+v, %v)", info, err)
		}
	})
}

// --- 5) 回归：storeLocal 多调用点不清零 LastSyncErr ---

func TestPhaseStoreLocalKeepsErr(t *testing.T) {
	key := bloomTestKey("phase-noclear")
	fn := func(context.Context, PrefillIngest) error { return nil }
	pf, _, mr := newPrefillFilterForTest(t, key, fn, WithSyncInterval(50*time.Millisecond))
	co := pf.coord

	// 成功一拍打底，再注入 WRONGTYPE 故障记录错误。
	if err := mr.Set(co.stateKey, pvReady); err != nil {
		t.Fatal(err)
	}
	co.syncOnce()
	mr.HSet(co.stateKey, "f", "v")
	co.syncOnce()
	info, _ := pf.Phase()
	if info.LastSyncErr == nil {
		t.Fatal("前置：故障拍应已记录 LastSyncErr")
	}
	errAt := info.LastSyncErrAt

	// 任意相位 storeLocal 调用点（syncOnce×2/finish×2 的形态）都不得
	// 连带清零 errSnapshot：逐一刷新相位并断言错误仍在。
	for _, p := range []PrefillPhase{PrefillUninitialized, PrefillBuilding, PrefillFailed, PrefillReady} {
		co.storeLocal(p)
		info, _ := pf.Phase()
		if info.LastSyncErr == nil || !info.LastSyncErrAt.Equal(errAt) {
			t.Fatalf("storeLocal(%v) 不得清零 LastSyncErr: %+v", p, info)
		}
		if info.Phase != p {
			t.Fatalf("storeLocal(%v) 后相位应同步可见，got %v", p, info.Phase)
		}
	}
}

// --- 6) 并发安全：Phase 与 syncOnce/storeLocal/readyFresh 并发（-race） ---

func TestPhaseConcurrent(t *testing.T) {
	key := bloomTestKey("phase-race")
	fn := func(context.Context, PrefillIngest) error { return nil }
	pf, _, mr := newPrefillFilterForTest(t, key, fn, WithSyncInterval(10*time.Millisecond))
	co := pf.coord
	if err := mr.Set(co.stateKey, pvReady); err != nil {
		t.Fatal(err)
	}

	const rounds = 200
	var wg sync.WaitGroup
	workers := []func(i int){
		func(int) { co.syncOnce() },
		func(int) { _, _ = pf.Phase() },
		func(int) { co.storeLocal(PrefillReady) },
		func(int) { _ = co.readyFresh() },
	}
	for _, w := range workers {
		wg.Add(1)
		go func(w func(int)) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				w(i)
			}
		}(w)
	}
	wg.Wait()
	waitInflightDone(co, 3*time.Second)
}

// --- 7) cuckoo 门面 Phase：真读 + 零 RTT（对照 bloom 口径一致） ---

func TestCuckooPhase(t *testing.T) {
	key := cuckooTestKey("phase-cf")
	fn := func(context.Context, PrefillIngest) error { return nil }
	cf, _, mr := newCuckooPrefillForTest(t, key, fn, WithSyncInterval(100*time.Millisecond))
	co := cf.coord

	if err := mr.Set(co.stateKey, pvReady); err != nil {
		t.Fatal(err)
	}
	co.syncOnce()

	n0 := mr.CommandCount()
	info, err := cf.Phase()
	if err != nil {
		t.Fatalf("cuckoo Phase: %v", err)
	}
	if got := mr.CommandCount() - n0; got != 0 {
		t.Fatalf("cuckoo Phase() 必须零 Redis 命令，实测多发 %d 条", got)
	}
	if info.Phase != PrefillReady || !info.Fresh || info.LastSyncErr != nil {
		t.Fatalf("cuckoo Phase 应 (Ready, Fresh, 无错): %+v", info)
	}
}

// --- 编译期护栏：Phase 已入 BloomFilter 接口（嵌入 Unimplemented 即满足） ---

var (
	_ BloomFilter = (*prefillFilter)(nil)
	_ BloomFilter = (*bfCmdImpl)(nil)
	_ BloomFilter = (*bitmapImpl)(nil)
)
