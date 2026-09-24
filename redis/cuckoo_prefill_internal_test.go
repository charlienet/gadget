package redis

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
)

// 布谷鸟预填充门控与降级（prefill）测试：未启用回归、降级分派、Reset
// 拦截、State 查询、ticker 自动重建冒烟。miniredis 无 CF.* 模块 → 工厂
// 走 hashImpl 路径；复用 bloom_prefill_internal_test.go 的同包 helper
// （newMiniRedisClient/waitInflightDone/mustGet/waitPrefillState/pv* 常量）。

// newCuckooPrefillForTest 经工厂构造启用 prefill 的布谷鸟过滤器（hashImpl
// 路径），构造后立即停后台同步 loop——测试手动驱动状态时序，避免 tick
// 自动重建与断言竞争；热路径惰性触发不依赖 loop，仍可测。
func newCuckooPrefillForTest(t *testing.T, key string, fn PrefillFunc, opts ...PrefillOption) (*CuckooFilter, *redisClient, *miniredis.Miniredis) {
	t.Helper()
	rc, mr := newMiniRedisClient(t)

	f, err := rc.NewCuckooFilter(t.Context(), key, WithCuckooPrefill(fn, opts...))
	if err != nil {
		t.Fatalf("NewCuckooFilter: %v", err)
	}
	if f.coord == nil {
		t.Fatalf("启用 WithCuckooPrefill 后应持有 coord（got nil）")
	}
	f.coord.stopLoop() // 只停后台同步 loop（不取消在飞 run）；测试手动驱动
	t.Cleanup(func() {
		waitInflightDone(f.coord, 3*time.Second)
		_ = f.Close()
	})
	return f, rc, mr
}

// --- 1) 未启用回归（无 WithCuckooPrefill / WithCuckooPrefill(nil)） ---

// TestCuckooPrefillDisabledRegression 验证不带 WithCuckooPrefill 时行为与
// 改动前一致：coord 为 nil、数据面/Reset 原语义、State=ErrPrefillDisabled、
// Close=nil；WithCuckooPrefill(nil) 静默不启用。
func TestCuckooPrefillDisabledRegression(t *testing.T) {
	ctx := t.Context()

	t.Run("工厂未启用行为原样", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		key := cuckooTestKey("cpf-off")
		f, err := rc.NewCuckooFilter(ctx, key, WithCuckooCapacity(10_000))
		if err != nil {
			t.Fatalf("NewCuckooFilter: %v", err)
		}
		if f.coord != nil {
			t.Fatal("未启用 WithCuckooPrefill 不应持有 coord")
		}

		// Add → Exists 真实查询
		added, err := f.Add(ctx, "off-1")
		if err != nil || !added {
			t.Fatalf("Add: added=%v err=%v", added, err)
		}
		if ok, err := f.Exists(ctx, "off-1"); err != nil || !ok {
			t.Fatalf("Exists 命中: ok=%v err=%v", ok, err)
		}
		// Count 原语义：存在=1
		if n, err := f.Count(ctx, "off-1"); err != nil || n != 1 {
			t.Fatalf("Count 存在项: n=%d err=%v", n, err)
		}
		// Del 原语义
		if del, err := f.Del(ctx, "off-1"); err != nil || !del {
			t.Fatalf("Del: del=%v err=%v", del, err)
		}
		if ok, err := f.Exists(ctx, "off-1"); err != nil || ok {
			t.Fatalf("Del 后应 miss: ok=%v err=%v", ok, err)
		}
		// Reset 原语义（纯清空，无 prefill 拦截）
		if _, err := f.Add(ctx, "off-2"); err != nil {
			t.Fatal(err)
		}
		if err := f.Reset(ctx); err != nil {
			t.Fatalf("Reset: %v", err)
		}
		if ok, err := f.Exists(ctx, "off-2"); err != nil || ok {
			t.Fatalf("Reset 后应清空: ok=%v err=%v", ok, err)
		}

		// State 未启用错误路径
		phase, err := f.State(ctx)
		if !errors.Is(err, ErrPrefillDisabled) || phase != PrefillUninitialized {
			t.Fatalf("未启用 State 应返回 (Uninitialized, ErrPrefillDisabled)，got (%v, %v)", phase, err)
		}
		// Close 未启用 = nil（幂等两次）
		if err := f.Close(); err != nil {
			t.Fatalf("未启用 Close 应 nil: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("重复 Close 应 nil: %v", err)
		}
	})

	t.Run("WithCuckooPrefill(nil) 静默不启用", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		f, err := rc.NewCuckooFilter(ctx, cuckooTestKey("cpf-nil"), WithCuckooPrefill(nil))
		if err != nil {
			t.Fatalf("NewCuckooFilter: %v", err)
		}
		if f.coord != nil {
			t.Fatal("WithCuckooPrefill(nil) 应静默忽略，不启用门控")
		}
		if p, err := f.State(ctx); !errors.Is(err, ErrPrefillDisabled) || p != PrefillUninitialized {
			t.Fatalf("WithCuckooPrefill(nil) State 应 disabled: got (%v, %v)", p, err)
		}
	})
}

// --- 2) 降级分派（非 Ready，coord.storeLocal 造相位） ---

// TestCuckooPrefillDegradeDispatch 验证门面降级分派：非 Ready 时
// Exists 恒 (true,nil)、ExistsMulti 非空全 true（空入参仍 (nil,nil)）、
// Count 恒 (1,nil)；Add/AddNX/AddMulti/Del 照常真实写入（miniredis/impl
// 回读验证）；Info 恒透传。相位用 storeLocal(Building) 造——Building
// 使 triggerLazy 短路，防止异步重建清空干扰写入回读。
func TestCuckooPrefillDegradeDispatch(t *testing.T) {
	ctx := t.Context()
	key := cuckooTestKey("cpf-dispatch")
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		_, err := ingest.AddMulti(ctx, "seed-1")
		return err
	}
	cf, _, mr := newCuckooPrefillForTest(t, key, fn,
		WithSyncInterval(100*time.Millisecond), WithRebuildTimeout(2*time.Second))
	co := cf.coord

	// 造非 Ready 相位（Building：本地降级 + triggerLazy 短路不发 acquire）
	co.storeLocal(PrefillBuilding)

	// Exists：非 Ready → 恒 (true,nil)，不经 FailPolicy
	if ok, err := cf.Exists(ctx, "never-1"); err != nil || !ok {
		t.Fatalf("非 Ready Exists 应恒 (true,nil): ok=%v err=%v", ok, err)
	}

	// ExistsMulti：非空 → 全 true 且 len=入参
	em, err := cf.ExistsMulti(ctx, "never-2", "never-3")
	if err != nil || len(em) != 2 || !em[0] || !em[1] {
		t.Fatalf("非 Ready ExistsMulti 应恒全 true+nil: v=%v err=%v", em, err)
	}
	// ExistsMulti：空入参先于降级判断走原路径 → (nil,nil)（R4 两态一致）
	if empty, err := cf.ExistsMulti(ctx); err != nil || empty != nil {
		t.Fatalf("非 Ready 空入参应 (nil,nil)，got (%v, %v)", empty, err)
	}

	// Count：非 Ready → 恒 (1,nil)（禁止返回 0）
	if n, err := cf.Count(ctx, "never-4"); err != nil || n != 1 {
		t.Fatalf("非 Ready Count 应恒 (1,nil): n=%d err=%v", n, err)
	}

	// Add：放行，照常真实写入（impl + miniredis 回读）
	added, err := cf.Add(ctx, "w1")
	if err != nil || !added {
		t.Fatalf("非 Ready Add 应照常写入: added=%v err=%v", added, err)
	}
	if ok, err := cf.impl.Exists(ctx, "w1"); err != nil || !ok {
		t.Fatalf("Add 后 impl 回读应命中: ok=%v err=%v", ok, err)
	}
	if !mr.Exists(key) {
		t.Fatal("Add 后 miniredis 应存在过滤器键")
	}

	// AddNX：放行且保持真实语义（已存在 → false；新项 → true）
	if nx, err := cf.AddNX(ctx, "w1"); err != nil || nx {
		t.Fatalf("非 Ready AddNX 已存在应 false: nx=%v err=%v", nx, err)
	}
	if nx, err := cf.AddNX(ctx, "w2"); err != nil || !nx {
		t.Fatalf("非 Ready AddNX 新项应 true: nx=%v err=%v", nx, err)
	}

	// AddMulti：放行，真实批量写入
	am, err := cf.AddMulti(ctx, "m1", "m2")
	if err != nil || len(am) != 2 || !am[0] || !am[1] {
		t.Fatalf("非 Ready AddMulti 应照常写入: v=%v err=%v", am, err)
	}
	got, err := cf.impl.ExistsMulti(ctx, "m1", "m2")
	if err != nil || len(got) != 2 || !got[0] || !got[1] {
		t.Fatalf("AddMulti 后 impl 回读应全命中: v=%v err=%v", got, err)
	}

	// Del：放行，真实删除
	del, err := cf.Del(ctx, "w1")
	if err != nil || !del {
		t.Fatalf("非 Ready Del 应照常删除: del=%v err=%v", del, err)
	}
	if ok, err := cf.impl.Exists(ctx, "w1"); err != nil || ok {
		t.Fatalf("Del 后 impl 回读应 miss: ok=%v err=%v", ok, err)
	}

	// Info：恒透传（观测类不降级）
	info, err := cf.Info(ctx)
	if err != nil || info == nil || info.NumBuckets <= 0 {
		t.Fatalf("Info 应透传真实值: info=%+v err=%v", info, err)
	}
	want, err := cf.impl.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.NumBuckets != want.NumBuckets || info.NumItems != want.NumItems {
		t.Fatalf("Info 应与 impl 一致: got=%+v want=%+v", info, want)
	}

	// 相位保持 Building（写入/查询不改相位、不发 acquire 干扰）
	if s := co.loadLocal(); s.phase != PrefillBuilding {
		t.Fatalf("断言期间本地相位应保持 Building，got %v", s.phase)
	}
}

// --- 3) Reset 拦截（force 全流程） ---

// TestCuckooPrefillResetIntercept 验证启用后 Reset = force 重建全流程：
// fn 被调用、完成后权威状态 ready、State 返回 PrefillReady、回灌生效、
// 清空语义（Reset 前写入被 inner.Reset 清掉）。
func TestCuckooPrefillResetIntercept(t *testing.T) {
	ctx := t.Context()
	key := cuckooTestKey("cpf-rst")
	var calls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		calls.Add(1)
		_, err := ingest.AddMulti(ctx, "rst-seed")
		return err
	}
	cf, _, mr := newCuckooPrefillForTest(t, key, fn,
		WithSyncInterval(20*time.Millisecond), WithRebuildTimeout(2*time.Second))

	// 先经 impl 直写一条（避免热路径惰性重建干扰；coord 已 stopLoop）
	if _, err := cf.impl.Add(ctx, "pre-1"); err != nil {
		t.Fatalf("impl.Add: %v", err)
	}

	// Reset = force 重建 全流程（acquire → Δ → inner.Reset → fn → ready）
	if err := cf.Reset(ctx); err != nil {
		t.Fatalf("Reset（=force 重建）: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("Reset 应经 prefill 流程调用 fn 一次，calls=%d", calls.Load())
	}
	if v := mustGet(t, mr, cf.coord.stateKey); v != pvReady {
		t.Fatalf("Reset 完成后权威状态应 ready，got %q", v)
	}
	if p, err := cf.State(ctx); err != nil || p != PrefillReady {
		t.Fatalf("Reset 后 State 应 PrefillReady: got (%v, %v)", p, err)
	}
	// 清空语义：pre-1 被 inner.Reset 清掉；回灌 rst-seed 生效
	if ok, err := cf.impl.Exists(ctx, "pre-1"); err != nil || ok {
		t.Fatalf("Reset 应清空重建前数据: ok=%v err=%v", ok, err)
	}
	if ok, err := cf.Exists(ctx, "rst-seed"); err != nil || !ok {
		t.Fatalf("Reset 后回灌项应命中: ok=%v err=%v", ok, err)
	}
}

// --- 4) State 查询与未启用错误路径 ---

// TestCuckooPrefillStateQuery 验证 State 1 RTT 权威查询：未启用返回
// ErrPrefillDisabled；启用后直接 GET 状态键三值映射（键缺失 →
// Uninitialized；ready/fail → 对应相位）。
func TestCuckooPrefillStateQuery(t *testing.T) {
	ctx := t.Context()

	t.Run("未启用错误路径", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		f, err := rc.NewCuckooFilter(ctx, cuckooTestKey("cpf-st-off"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := f.State(ctx)
		if !errors.Is(err, ErrPrefillDisabled) || p != PrefillUninitialized {
			t.Fatalf("未启用 State: got (%v, %v)", p, err)
		}
	})

	t.Run("启用后权威三值", func(t *testing.T) {
		fn := func(ctx context.Context, ingest PrefillIngest) error { return nil }
		cf, rc, _ := newCuckooPrefillForTest(t, cuckooTestKey("cpf-st"), fn)
		sk := cf.coord.stateKey

		// 键缺失 → Uninitialized
		if p, err := cf.State(ctx); err != nil || p != PrefillUninitialized {
			t.Fatalf("键缺失 State 应 Uninitialized: got (%v, %v)", p, err)
		}
		// ready
		if err := rc.Set(ctx, sk, pvReady, 0).Err(); err != nil {
			t.Fatal(err)
		}
		if p, err := cf.State(ctx); err != nil || p != PrefillReady {
			t.Fatalf("ready State: got (%v, %v)", p, err)
		}
		// fail
		if err := rc.Set(ctx, sk, pvFail, 0).Err(); err != nil {
			t.Fatal(err)
		}
		if p, err := cf.State(ctx); err != nil || p != PrefillFailed {
			t.Fatalf("fail State: got (%v, %v)", p, err)
		}
	})
}

// --- 5) 冒烟：ticker 自动重建链 ---

// TestCuckooPrefillTickerAutoRebuild 验证工厂启用后 coord 存在、后台
// ticker 自动重建链（短 syncInterval、不关 loop、无人工介入）：状态限期
// 自动到 ready、fn 至少一次、本地相位 Ready——参考
// TestBloomPrefillTickerAutoRebuild 模式。
func TestCuckooPrefillTickerAutoRebuild(t *testing.T) {
	rc, mr := newMiniRedisClient(t)
	t.Cleanup(func() { _ = rc.Close() })
	ctx := t.Context()
	key := cuckooTestKey("cpf-tick")

	var calls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		calls.Add(1)
		_, err := ingest.AddMulti(ctx, "tick-seed")
		return err
	}
	f, err := rc.NewCuckooFilter(ctx, key,
		WithCuckooPrefill(fn, WithSyncInterval(20*time.Millisecond), WithRebuildTimeout(2*time.Second)))
	if err != nil {
		t.Fatalf("NewCuckooFilter: %v", err)
	}
	if f.coord == nil {
		t.Fatal("工厂启用后应持有 coord")
	}
	// 不 stopLoop：保留后台 loop 自动驱动；用后清理。
	t.Cleanup(func() {
		waitInflightDone(f.coord, 3*time.Second)
		_ = f.Close()
	})

	// jitter(<20ms)+首 tick(≤40ms)+Δ(40ms)+fn → 5s 上限足够；超时即链路断裂。
	waitPrefillState(t, mr, f.coord.stateKey, pvReady, 5*time.Second)
	if n := calls.Load(); n < 1 {
		t.Fatalf("自动重建应至少执行 fn 一次，calls=%d", n)
	}
	// C6：本地相位用 eventually（权威 ready 与 storeLocal 间有固有窗口）。
	waitLocalPhase(t, f.coord, PrefillReady, time.Second)
	if p, err := f.State(ctx); err != nil || p != PrefillReady {
		t.Fatalf("State 应权威 Ready: got (%v, %v)", p, err)
	}
	// 回灌生效（Ready 真实查询）
	if ok, err := f.Exists(ctx, "tick-seed"); err != nil || !ok {
		t.Fatalf("回灌项应命中: ok=%v err=%v", ok, err)
	}
}

// --- C4-①: FailPolicy 正交性（非 Ready 降级规则 vs 服务失效兜底） ---

// TestCuckooPrefillFailPolicyOrthogonal 验证 FailClosed + 服务故障注入
// （miniredis 关闭 → dial 失败 → IsUnavailable）下两套规则正交：
// 非 Ready 的 Exists/ExistsMulti/Count 是业务规则，恒降级返回且 err==nil
// （不经 FailPolicy、不发命令）；Add 放行走原路径，服务失效按 FailPolicy
// 兜底（FailClosed → false + ErrRedisUnavailable 哨兵）。
// 判别力：破坏 cuckoo.go Count 非 Ready 分支 return 1→0 时本用例变红。
func TestCuckooPrefillFailPolicyOrthogonal(t *testing.T) {
	ctx := t.Context()
	rc, mr := newMiniRedisClient(t)
	key := cuckooTestKey("cpf-fp")
	fn := func(ctx context.Context, ingest PrefillIngest) error { return nil }

	f, err := rc.NewCuckooFilter(ctx, key,
		WithCuckooPrefill(fn, WithSyncInterval(time.Hour)),
		WithFailPolicy[*cuckooConfig](FailClosed))
	if err != nil {
		t.Fatalf("NewCuckooFilter: %v", err)
	}
	if f.coord == nil {
		t.Fatal("应启用 coord")
	}
	f.coord.stopLoop() // 手动驱动时序（helper 签名只收 PrefillOption，本用例手工构造）
	t.Cleanup(func() {
		waitInflightDone(f.coord, 3*time.Second)
		_ = f.Close()
	})

	// 造非 Ready 相位（Building：降级 + triggerLazy 短路，防故障期后台发令）
	f.coord.storeLocal(PrefillBuilding)

	// 故障注入：关闭 miniredis（dial 失败 → ErrRedisUnavailable；包内先例
	// 见 bloom_internal_test.go TestLuaSupportStateTransition、failover_test.go）
	mr.Close()

	// 降级规则不受 FailPolicy 影响：Exists 恒 (true, nil)——不发命令
	if ok, err := f.Exists(ctx, "fp-1"); err != nil || !ok {
		t.Fatalf("非 Ready+FailClosed Exists 应恒 (true,nil): ok=%v err=%v", ok, err)
	}
	// ExistsMulti 非空恒全 true + nil
	em, err := f.ExistsMulti(ctx, "fp-1", "fp-2")
	if err != nil || len(em) != 2 || !em[0] || !em[1] {
		t.Fatalf("非 Ready+FailClosed ExistsMulti 应恒全 true+nil: v=%v err=%v", em, err)
	}
	// Count 恒 (1, nil)（降级规则：禁止 0；破坏 return 1→0 即红）
	if n, err := f.Count(ctx, "fp-1"); err != nil || n != 1 {
		t.Fatalf("非 Ready+FailClosed Count 应恒 (1,nil): n=%d err=%v", n, err)
	}

	// Add 放行原路径 → 服务失效 → FailPolicy 兜底（FailClosed=false+哨兵）
	added, err := f.Add(ctx, "fp-w")
	if !errors.Is(err, ErrRedisUnavailable) {
		t.Fatalf("FailClosed+宕机 Add 应返回 ErrRedisUnavailable 哨兵: added=%v err=%v", added, err)
	}
	if added {
		t.Fatalf("FailClosed 兜底值应 false: got %v", added)
	}
	// 对照组：同故障下 Ready 降级规则不适用——非 Ready 断言已覆盖（相位保持 Building）
	if s := f.coord.loadLocal(); s.phase != PrefillBuilding {
		t.Fatalf("断言期间本地相位应保持 Building，got %v", s.phase)
	}
}

// --- C4-②: cfCmdImpl 模块版 Reset 拦截全流程（真 CF.* 服务器 gate） ---

// TestCuckooPrefillCfCmdResetRealRedis 在真实 Redis（REDIS_URL）且服务器
// 加载 RedisBloom cuckoo 模块（Capability().HasCuckoo()）时，经工厂分派
// cfCmdImpl 路径跑 Reset 拦截全流程：force 重建 → fn 被调 → 权威/State
// ready → 回灌项命中。无环境或无模块 Skip（包内既有 integration gate 纪律）。
func TestCuckooPrefillCfCmdResetRealRedis(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skip real-Redis CF prefill test")
	}
	rdb, err := NewWithURL(url)
	if err != nil {
		t.Fatalf("NewWithURL: %v", err)
	}
	t.Cleanup(func() { _ = rdb.GracefulClose(context.Background()) })
	_ = rdb.Capability().Probe(t.Context()) // 查询为纯内存读：guard 前显式探测
	if !rdb.Capability().HasCuckoo() {
		t.Skip("server has no CF.* module; skip cfCmdImpl prefill test")
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}

	ctx := context.Background()
	key := cuckooTestKey("cpf-cf")
	stateKey, failnKey := prefillStateKeys(key)
	t.Cleanup(func() { _ = rc.Del(ctx, key, stateKey, failnKey).Err() }) // 收尾 Del，严禁 FLUSHDB

	var calls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		calls.Add(1)
		_, err := ingest.AddMulti(ctx, "cf-seed")
		return err
	}
	cf, err := rc.NewCuckooFilter(ctx, key,
		WithCuckooPrefill(fn, WithSyncInterval(50*time.Millisecond), WithRebuildTimeout(10*time.Second)),
		WithCuckooCapacity(10_000))
	if err != nil {
		t.Fatalf("NewCuckooFilter: %v", err)
	}
	if cf.coord == nil {
		t.Fatal("启用后应持有 coord")
	}
	t.Cleanup(func() { _ = cf.Close() })
	// 白盒确认分派到模块版（HasCuckoo 为真时工厂必选 cfCmdImpl）
	if _, isCF := cf.impl.(*cfCmdImpl); !isCF {
		t.Fatalf("HasCuckoo 下应分派 cfCmdImpl，got %T", cf.impl)
	}

	// Reset 拦截 = force 全流程（cfCmdImpl 的 inner.Reset = DEL + CF.RESERVE）
	if err := cf.Reset(ctx); err != nil {
		t.Fatalf("Reset（=force 重建）: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("fn 应被调一次，calls=%d", calls.Load())
	}
	if p, err := cf.State(ctx); err != nil || p != PrefillReady {
		t.Fatalf("Reset 后 State 应 PrefillReady: got (%v, %v)", p, err)
	}
	if ok, err := cf.Exists(ctx, "cf-seed"); err != nil || !ok {
		t.Fatalf("回灌项应命中: ok=%v err=%v", ok, err)
	}
}

// --- C4-③: Close 幂等与 GracefulClose 级联 ---

// TestCuckooPrefillCloseIdempotent 覆盖协调器生命周期（手法参考 bloom
// TestBloomPrefillCloseCascade）：双次 cf.Close() 幂等且停 loop；
// rc.GracefulClose 经 closeState 级联停 ticker，重复 Close/GracefulClose
// 均幂等。
func TestCuckooPrefillCloseIdempotent(t *testing.T) {
	fn := func(ctx context.Context, ingest PrefillIngest) error { return nil }

	t.Run("双次 Close 幂等", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		f, err := rc.NewCuckooFilter(t.Context(), cuckooTestKey("cpf-cl"),
			WithCuckooPrefill(fn, WithSyncInterval(time.Hour)))
		if err != nil {
			t.Fatalf("NewCuckooFilter: %v", err)
		}
		if f.coord == nil {
			t.Fatal("应启用 coord")
		}
		// 不 stopLoop：保留 loop 运行，由 Close 停（判别力在 stopCh）
		if err := f.Close(); err != nil {
			t.Fatalf("首次 Close: %v", err)
		}
		select {
		case <-f.coord.stopCh:
		default:
			t.Fatal("Close 应停 loop（stopCh 关闭）")
		}
		if err := f.Close(); err != nil {
			t.Fatalf("二次 Close 应幂等: %v", err)
		}
	})

	t.Run("GracefulClose 级联停 loop", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		f, err := rc.NewCuckooFilter(t.Context(), cuckooTestKey("cpf-cl2"),
			WithCuckooPrefill(fn, WithSyncInterval(time.Hour)))
		if err != nil {
			t.Fatalf("NewCuckooFilter: %v", err)
		}
		if f.coord == nil {
			t.Fatal("应启用 coord")
		}
		// coordinator 注册进 closeState 级联表
		rc.state.mu.Lock()
		n := len(rc.state.closers)
		rc.state.mu.Unlock()
		if n == 0 {
			t.Fatal("coordinator 应注册进 closeState.closers")
		}
		if err := rc.GracefulClose(t.Context()); err != nil {
			t.Fatalf("GracefulClose: %v", err)
		}
		select {
		case <-f.coord.stopCh:
		default:
			t.Fatal("GracefulClose 应停 coordinator ticker（stopCh 关闭）")
		}
		// 幂等：Close 与重复 GracefulClose
		if err := f.Close(); err != nil {
			t.Fatalf("重复 Close: %v", err)
		}
		if err := rc.GracefulClose(t.Context()); err != nil {
			t.Fatalf("重复 GracefulClose: %v", err)
		}
	})
}

// --- C2: prefillProbe 透传调用方 ctx values ---

// TestCuckooPrefillProbeCtxValues 验证 prefillProbe(ctx) 与 bloom
// maybeTrigger(ctx) 同构：冷启动首请求带 values 的 ctx 触发惰性重建后，
// fn 收到的 ctx（triggerLazy 经 WithoutCancel 派生）仍可读到调用方
// values（trace/租户等不丢失）。
func TestCuckooPrefillProbeCtxValues(t *testing.T) {
	type ctxKey string
	const k ctxKey = "cuckoo-prefill-ctx" // 自定义类型 key，避免跨包碰撞
	fnDone := make(chan any, 1)
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		select { // 只取首次调用的 values
		case fnDone <- ctx.Value(k):
		default:
		}
		_, err := ingest.AddMulti(ctx, "ctx-seed")
		return err
	}
	cf, _, mr := newCuckooPrefillForTest(t, cuckooTestKey("cpf-ctx"), fn,
		WithSyncInterval(20*time.Millisecond), WithRebuildTimeout(2*time.Second))

	// 冷启动本地 Uninitialized：带 values 的调用方 ctx 走查询面触发惰性重建
	vctx := context.WithValue(t.Context(), k, "tenant-42")
	if ok, err := cf.Exists(vctx, "probe-1"); err != nil || !ok {
		t.Fatalf("非 Ready Exists 应 (true,nil): ok=%v err=%v", ok, err)
	}
	waitPrefillState(t, mr, cf.coord.stateKey, pvReady, 5*time.Second)
	select {
	case v := <-fnDone:
		if v != "tenant-42" {
			t.Fatalf("fn ctx 应读到调用方 values: got %v want tenant-42", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fn 未在限期内执行（惰性重建链断裂）")
	}
}
