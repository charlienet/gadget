package redis

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
)

// Bloom 预填充门控与降级（prefill）测试 A：helper、键协议、选项、
// 未启用回归、降级分流、状态机迁移。
// miniredis 无 BF.* 模块 → 工厂走 bitmap 路径；并发互斥类断言依赖真
// Redis Lua 原子性（miniredis EVAL 非原子，见 bloom_internal_test.go:319），
// 按包内既有 integration 纪律 gate 到 REDIS_URL（见文件 B）。

// 状态键三值字面量（协议见 bloom_prefill.go；测试自含，不依赖实现常量）。
const (
	pvReady    = "ready"
	pvBuilding = "building"
	pvFail     = "fail"
)

// newPrefillFilterForTest 经工厂构造启用 prefill 的过滤器（bitmap 路径），
// 构造后立即停后台同步 loop——测试手动驱动状态时序，避免 tick 自动重建
// 与断言竞争；热路径惰性触发不依赖 loop，仍可测。capacity 取小加快建键；
// FP 取 0.0001 使"Ready 后真实查询 miss=false"几乎无假阳噪声。
func newPrefillFilterForTest(t *testing.T, key string, fn PrefillFunc, opts ...PrefillOption) (*prefillFilter, *redisClient, *miniredis.Miniredis) {
	t.Helper()
	rc, mr := newMiniRedisClient(t)

	bopts := []BloomOption{
		WithCapacity(10_000),
		WithFalsePositive(0.0001),
		WithPrefill(fn, opts...),
	}
	f, err := rc.NewBloomFilter(t.Context(), key, bopts...)
	if err != nil {
		t.Fatalf("NewBloomFilter: %v", err)
	}
	pf, ok := f.(*prefillFilter)
	if !ok {
		t.Fatalf("启用 WithPrefill 后应返回 *prefillFilter，got %T", f)
	}
	pf.coord.stopLoop() // 只停后台同步 loop（不取消在飞 run，见 R5）；测试手动驱动
	t.Cleanup(func() {
		// 先等后台重建完全退出再关连接：消除 cleanup 关闭序列与在飞命令
		// 并发时 miniredis v2.5.0 Close（WaitGroup×mu）的间歇死锁。
		// inFlight 清零于 run 返回前，其后无在飞命令；安静等待不 Fatal。
		waitInflightDone(pf.coord, 3*time.Second)
		_ = pf.Close()
	})
	return pf, rc, mr
}

// waitInflightDone 安静等待后台重建退出（cleanup 专用，不 Fatal——超时
// 仅放弃等待交由后续关闭自行兜底）。
func waitInflightDone(co *prefillCoordinator, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !co.inFlight.Load() {
			time.Sleep(10 * time.Millisecond) // 留一拍让 run goroutine 真正退出
			if !co.inFlight.Load() {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// mustGet 读 miniredis 内存键值，不存在即 Fatal。
func mustGet(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mr.Get(key)
	if err != nil {
		t.Fatalf("mr.Get(%s): %v", key, err)
	}
	return v
}

// waitPrefillState 轮询等待状态键达到期望值；want=="" 表示等待键缺失。
func waitPrefillState(t *testing.T, mr *miniredis.Miniredis, stateKey, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		v, err := mr.Get(stateKey)
		if want == "" {
			if err != nil {
				return
			}
		} else if err == nil && v == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	v, err := mr.Get(stateKey)
	t.Fatalf("等待状态键 %s=%q 超时：last=%q err=%v", stateKey, want, v, err)
}

// waitLocalPhase 限期轮询协调器本地相位直至达到期望值（eventually 语义：
// run 的 storeLocal(Ready) 与权威 GET-store 之间存在固有非原子窗口，
// 立即断言会 flaky——见 TickerAutoRebuild 用例），每 5ms 一次；超时
// Fatal 并打印末次相位（C6）。
func waitLocalPhase(t *testing.T, co *prefillCoordinator, want PrefillPhase, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := PrefillPhase(255) // 哨兵：未曾读到任何快照
	for time.Now().Before(deadline) {
		last = co.loadLocal().phase
		if last == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待本地相位 %v 超时（%v 内）：末次相位=%v", want, timeout, last)
}

// --- 键协议（规格 §1） ---

// TestBloomPrefillKeyProtocol 验证 tagbase 包裹规则与两键命名：
// 无有效 hash tag 的 base 自动包裹 {base}；首个 '{' 后含非空 '}' 的
// 自带 tag 原样；状态键/计数键同前缀同 hash tag（Cluster 同 slot 前提）。
func TestBloomPrefillKeyProtocol(t *testing.T) {
	s, f := prefillStateKeys("plain")
	if s != "{plain}:__prefill" {
		t.Fatalf("无 hash tag 应包裹：got %q", s)
	}
	if f != "{plain}:__prefill:failn" {
		t.Fatalf("计数键：got %q", f)
	}

	s2, f2 := prefillStateKeys("app{tag}x")
	if s2 != "app{tag}x:__prefill" || f2 != "app{tag}x:__prefill:failn" {
		t.Fatalf("自带 hash tag 应原样：got %q / %q", s2, f2)
	}

	// R1：未闭合 hash tag（x{y 无 '}'）不构成有效 tag，必须包裹，
	// 否则两键按整键算 slot → Cluster 双键 EVAL CROSSSLOT。
	s3, f3 := prefillStateKeys("x{y")
	if s3 != "{x{y}:__prefill" || f3 != "{x{y}:__prefill:failn" {
		t.Fatalf("未闭合 { 应视为无 tag 并包裹：got %q / %q", s3, f3)
	}

	// R1 收尾：空 hash tag（a{}b 的 {} 无内容）不构成有效 tag，应包裹
	// 而非透传——透传则两键取整键算 slot 不同 → CROSSSLOT。
	s4, f4 := prefillStateKeys("a{}b")
	if s4 != "{a{}b}:__prefill" || f4 != "{a{}b}:__prefill:failn" {
		t.Fatalf("空 {} 应视为无 tag 并包裹：got %q / %q", s4, f4)
	}

	// 两键 hash tag 提取一致（同 slot 前提）
	if tagOf(s) != tagOf(f) || tagOf(s) != "plain" {
		t.Fatalf("两键 hash tag 应一致：state=%q failn=%q", tagOf(s), tagOf(f))
	}
	if tagOf(s2) != "tag" || tagOf(f2) != "tag" {
		t.Fatalf("自带 tag 提取：state=%q failn=%q", tagOf(s2), tagOf(f2))
	}
	if tagOf(s3) != tagOf(f3) || tagOf(s3) == "" {
		t.Fatalf("包裹后两键 hash tag 应一致且非空：state=%q failn=%q", tagOf(s3), tagOf(f3))
	}
	if tagOf(s4) != tagOf(f4) || tagOf(s4) == "" {
		t.Fatalf("空 tag 包裹后两键 hash tag 应一致且非空：state=%q failn=%q", tagOf(s4), tagOf(f4))
	}
}

// tagOf 提取键名首个 {...} 内容（纯测试辅助，与 CRC/slot 计算无关）。
func tagOf(key string) string {
	start, end := -1, -1
	for i, c := range key {
		if c == '{' && start < 0 {
			start = i
		}
		if c == '}' && start >= 0 {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		return ""
	}
	return key[start+1 : end]
}

// --- 选项（规格 §5：非法值静默忽略、默认值） ---

func TestBloomPrefillOptionBounds(t *testing.T) {
	cfg := defaultBloomConfig()
	if cfg.enabled {
		t.Fatal("默认不应启用 prefill")
	}
	if cfg.rebuildTimeout != 5*time.Minute {
		t.Fatalf("默认 rebuildTimeout: %v", cfg.rebuildTimeout)
	}
	if cfg.retryInitial != 5*time.Second || cfg.retryMax != 10*time.Minute {
		t.Fatalf("默认 backoff: %v / %v", cfg.retryInitial, cfg.retryMax)
	}
	if cfg.syncInterval != time.Second {
		t.Fatalf("默认 syncInterval: %v", cfg.syncInterval)
	}

	// 非法值静默忽略
	WithRebuildTimeout(0)(&cfg.prefillConfig)
	WithRebuildTimeout(-time.Second)(&cfg.prefillConfig)
	WithSyncInterval(0)(&cfg.prefillConfig)
	WithSyncInterval(-time.Second)(&cfg.prefillConfig)
	WithRetryBackoff(0, 0)(&cfg.prefillConfig)
	WithRetryBackoff(-time.Second, -time.Minute)(&cfg.prefillConfig)
	WithPrefill(nil)(&cfg)
	if cfg.rebuildTimeout != 5*time.Minute || cfg.syncInterval != time.Second ||
		cfg.retryInitial != 5*time.Second || cfg.retryMax != 10*time.Minute || cfg.enabled {
		t.Fatalf("非法值应静默忽略：to=%v si=%v bo=%v/%v en=%v",
			cfg.rebuildTimeout, cfg.syncInterval, cfg.retryInitial, cfg.retryMax, cfg.enabled)
	}

	// 合法值采纳
	WithRebuildTimeout(30 * time.Second)(&cfg.prefillConfig)
	WithSyncInterval(50 * time.Millisecond)(&cfg.prefillConfig)
	WithRetryBackoff(100*time.Millisecond, 3*time.Second)(&cfg.prefillConfig)
	if cfg.rebuildTimeout != 30*time.Second || cfg.syncInterval != 50*time.Millisecond ||
		cfg.retryInitial != 100*time.Millisecond || cfg.retryMax != 3*time.Second {
		t.Fatalf("合法值未采纳：to=%v si=%v bo=%v/%v",
			cfg.rebuildTimeout, cfg.syncInterval, cfg.retryInitial, cfg.retryMax)
	}

	// WithPrefill(fn) 启用
	WithPrefill(func(context.Context, PrefillIngest) error { return nil })(&cfg)
	if !cfg.enabled || cfg.fn == nil {
		t.Fatal("WithPrefill(fn) 应启用")
	}
}

// --- 1) 未启用回归（规格 §8-1） ---

// TestBloomPrefillDisabledRegression 验证不带 WithPrefill 时行为与改动前
// 一致：不被包装、数据面/Reset 原语义、State=ErrPrefillDisabled。
func TestBloomPrefillDisabledRegression(t *testing.T) {
	ctx := t.Context()

	t.Run("工厂未启用", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		f, err := rc.NewBloomFilter(ctx, bloomTestKey("pf-off"), WithCapacity(10_000), WithFalsePositive(0.0001))
		if err != nil {
			t.Fatalf("NewBloomFilter: %v", err)
		}
		if _, ok := f.(*prefillFilter); ok {
			t.Fatal("未启用 WithPrefill 不应返回 *prefillFilter")
		}
		if _, ok := f.(*bitmapImpl); !ok {
			t.Fatalf("miniredis 无 bf 模块应分派 bitmapImpl，got %T", f)
		}

		added, err := f.Add(ctx, "off-1")
		if err != nil || !added {
			t.Fatalf("Add: added=%v err=%v", added, err)
		}
		if ok, err := f.Exists(ctx, "off-1"); err != nil || !ok {
			t.Fatalf("Exists 命中: ok=%v err=%v", ok, err)
		}
		if err := f.Reset(ctx); err != nil {
			t.Fatalf("Reset: %v", err)
		}
		if ok, err := f.Exists(ctx, "off-1"); err != nil || ok {
			t.Fatalf("Reset 后应清空: ok=%v err=%v", ok, err)
		}

		phase, err := f.State(ctx)
		if !errors.Is(err, ErrPrefillDisabled) || phase != PrefillUninitialized {
			t.Fatalf("未启用 State 应返回 (Uninitialized, ErrPrefillDisabled)，got (%v, %v)", phase, err)
		}
	})

	t.Run("两 impl 空实现", func(t *testing.T) {
		for _, impl := range []BloomFilter{&bfCmdImpl{}, &bitmapImpl{}} {
			if p, err := impl.State(ctx); !errors.Is(err, ErrPrefillDisabled) || p != PrefillUninitialized {
				t.Fatalf("%T.State: got (%v, %v)", impl, p, err)
			}
		}
	})

	t.Run("UnimplementedBloomFilter", func(t *testing.T) {
		var u UnimplementedBloomFilter
		if p, err := u.State(ctx); !errors.Is(err, ErrPrefillDisabled) || p != PrefillUninitialized {
			t.Fatalf("State: got (%v, %v)", p, err)
		}
	})

	t.Run("WithPrefill(nil) 静默忽略", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		f, err := rc.NewBloomFilter(ctx, bloomTestKey("pf-nil"), WithPrefill(nil))
		if err != nil {
			t.Fatalf("NewBloomFilter: %v", err)
		}
		if _, ok := f.(*prefillFilter); ok {
			t.Fatal("WithPrefill(nil) 应静默忽略，不启用门控")
		}
	})
}

// --- 2) 降级分流（规格 §8-2） ---

// TestBloomPrefillDegradeDispatch 验证数据面分派：非 Ready 时 Add 照常
// 写入、Exists/ExistsMulti 恒全 true+nil、Info/Card 透传；Ready 后恢复
// 真实查询；冷启动首请求触发惰性重建。
func TestBloomPrefillDegradeDispatch(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("pf-dispatch")
	var calls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		calls.Add(1)
		_, err := ingest.AddMulti(ctx, "seed-1")
		return err
	}
	pf, _, mr := newPrefillFilterForTest(t, key, fn, WithSyncInterval(100*time.Millisecond), WithRebuildTimeout(2*time.Second))
	co := pf.coord

	// 本地初值：Uninitialized + updatedAt 零值（首请求即降级+可触发）
	if s := co.loadLocal(); s.phase != PrefillUninitialized || !s.updatedAt.IsZero() {
		t.Fatalf("本地初值应 Uninitialized+零时间，got %+v", s)
	}

	// 降级阶段断言（惰性 Δ=200ms，以下同步断言毫秒级完成）
	added, err := pf.Add(ctx, "hit-1")
	if err != nil || !added {
		t.Fatalf("Add 应照常透传: added=%v err=%v", added, err)
	}
	if ok, err := pf.inner.Exists(ctx, "hit-1"); err != nil || !ok {
		t.Fatalf("Add 后 inner 写入应真实发生: ok=%v err=%v", ok, err)
	}
	if ok, err := pf.Exists(ctx, "never-1"); err != nil || !ok {
		t.Fatalf("非 Ready Exists 应恒 (true,nil): ok=%v err=%v", ok, err)
	}
	am, err := pf.AddMulti(ctx, "m-1", "m-2")
	if err != nil || len(am) != 2 || !am[0] || !am[1] {
		t.Fatalf("AddMulti 应照常透传: v=%v err=%v", am, err)
	}
	em, err := pf.ExistsMulti(ctx, "m-1", "never-2")
	if err != nil || len(em) != 2 || !em[0] || !em[1] {
		t.Fatalf("非 Ready ExistsMulti 应恒全 true+nil: v=%v err=%v", em, err)
	}
	// R4：空入参两态一致——非 Ready 也透传 inner（inner 空入参返回 nil，
	// 不得返回空非 nil 切片）。
	if empty, err := pf.ExistsMulti(ctx); err != nil || empty != nil {
		t.Fatalf("非 Ready 空入参应与 inner 一致 (nil,nil)，got (%v, %v)", empty, err)
	}
	info, err := pf.Info(ctx)
	if err != nil || info.Capacity != 10_000 {
		t.Fatalf("Info 应透传: info=%+v err=%v", info, err)
	}
	card, err := pf.Card(ctx)
	if err != nil || card < 1 {
		t.Fatalf("Card 应透传真实值（已 Add ≥1）: card=%d err=%v", card, err)
	}

	// 冷启动惰性触发：后台 acquire → Δ → Reset → fn → ready
	waitPrefillState(t, mr, co.stateKey, pvBuilding, 2*time.Second)
	waitPrefillState(t, mr, co.stateKey, pvReady, 3*time.Second)
	co.storeLocal(PrefillReady) // 刷新 updatedAt，消除 stale 竞态后再断言真实查询

	// Ready 后恢复真实查询
	if ok, err := pf.Exists(ctx, "never-3"); err != nil || ok {
		t.Fatalf("Ready 后 Exists miss 应恢复真实 false: ok=%v err=%v", ok, err)
	}
	if ok, err := pf.Exists(ctx, "seed-1"); err != nil || !ok {
		t.Fatalf("Ready 后回灌项应命中: ok=%v err=%v", ok, err)
	}
	em2, err := pf.ExistsMulti(ctx, "seed-1", "never-4")
	if err != nil || len(em2) != 2 || !em2[0] || em2[1] {
		t.Fatalf("Ready 后 ExistsMulti 应真实: v=%v err=%v", em2, err)
	}
	if calls.Load() < 1 {
		t.Fatalf("惰性重建应至少执行 fn 一次，calls=%d", calls.Load())
	}
}

// --- 3) 状态机迁移（规格 §8-3、§8-4） ---

// TestBloomPrefillStateMachine 覆盖 T1/T2/T3/T4/T5/T7/T8/T9：
// Uninitialized→Reset(force)→Ready；失败→fail 且 TTL=backoff(1)；退避窗口
// force=0 抢占失败；FastForward 过期后第二次失败 backoff(2)；force=1
// 穿透退避并归零 failn；Building 中显式 Reset 拒绝；building TTL 过期
// 回 Uninitialized；force 从 Ready 抢占、force=0 不抢占 Ready；成功后
// failn 归零。
func TestBloomPrefillStateMachine(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("pf-sm")
	var calls atomic.Int32
	var failing atomic.Bool
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		calls.Add(1)
		if failing.Load() {
			return errors.New("prefill boom")
		}
		_, err := ingest.AddMulti(ctx, "sm-seed")
		return err
	}
	pf, rc, mr := newPrefillFilterForTest(t, key, fn,
		WithSyncInterval(20*time.Millisecond),
		WithRebuildTimeout(time.Second),
		WithRetryBackoff(300*time.Millisecond, 10*time.Second))
	co := pf.coord
	b1, b2 := 300*time.Millisecond, 600*time.Millisecond

	// T1: Uninitialized → Reset(force=1) → Ready；成功后 failn 不存在
	if err := pf.Reset(ctx); err != nil {
		t.Fatalf("T1 Reset: %v", err)
	}
	if v := mustGet(t, mr, co.stateKey); v != pvReady {
		t.Fatalf("T1 完成后状态应 ready，got %q", v)
	}
	if s := co.loadLocal(); s.phase != PrefillReady {
		t.Fatalf("就地更新本地相位应 Ready，got %v", s.phase)
	}
	if mr.Exists(co.failnKey) {
		t.Fatal("T1 成功后 failn 应不存在")
	}

	// T2: force=0 绝不抢占 Ready（静默、fn 不触发、状态不变）
	calls.Store(0)
	if err := co.run(ctx, 0); err != nil {
		t.Fatalf("force=0 on ready 应静默: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("force=0 不应触发 fn，calls=%d", calls.Load())
	}
	if v := mustGet(t, mr, co.stateKey); v != pvReady {
		t.Fatalf("force=0 不应抢占 ready，got %q", v)
	}

	// T5: Building 中显式 Reset → ErrRebuildInProgress
	if err := rc.Set(ctx, co.stateKey, pvBuilding, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := pf.Reset(ctx); !errors.Is(err, ErrRebuildInProgress) {
		t.Fatalf("T5 Building 中 Reset 应返回 ErrRebuildInProgress，got %v", err)
	}

	// T7 准备：回到 Uninitialized，fn 转失败
	if err := rc.Del(ctx, co.stateKey).Err(); err != nil {
		t.Fatal(err)
	}
	failing.Store(true)
	calls.Store(0)
	if err := pf.Reset(ctx); err == nil {
		t.Fatal("fn 失败时 Reset 应返回错误")
	}
	if calls.Load() != 1 {
		t.Fatalf("失败 Reset 应执行 fn 一次，calls=%d", calls.Load())
	}
	if v := mustGet(t, mr, co.stateKey); v != pvFail {
		t.Fatalf("失败后状态应 fail，got %q", v)
	}
	// R8：区间断言 [b1-50ms, b1]——既防过长也抓过短（回退到默认/更小值）。
	if ttl := mr.TTL(co.stateKey); ttl < b1-50*time.Millisecond || ttl > b1 {
		t.Fatalf("fail TTL 应落在 backoff(1)=%v±50ms，got %v", b1, ttl)
	}
	if v := mustGet(t, mr, co.failnKey); v != "1" {
		t.Fatalf("首次失败 failn 应为 1，got %q", v)
	}
	if s := co.loadLocal(); s.phase != PrefillFailed {
		t.Fatalf("失败后本地相位应 Failed，got %v", s.phase)
	}

	// T3: 退避窗口内 force=0 抢占失败 → 静默不动（fn 不执行、状态不变）
	calls.Store(0)
	if err := co.run(ctx, 0); err != nil {
		t.Fatalf("退避窗口 force=0 应静默: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("退避窗口不应执行 fn，calls=%d", calls.Load())
	}
	if v := mustGet(t, mr, co.stateKey); v != pvFail {
		t.Fatalf("退避窗口状态应保持 fail，got %q", v)
	}

	// T9: FastForward 过退避 → fail 键过期回 Uninitialized（failn 保留 24h）
	mr.FastForward(b1 + 10*time.Millisecond)
	if mr.Exists(co.stateKey) {
		t.Fatal("backoff 过期后 fail 状态键应消失")
	}
	if !mr.Exists(co.failnKey) {
		t.Fatal("failn 应保留（EXPIRE 86400）")
	}
	if p, err := pf.State(ctx); err != nil || p != PrefillUninitialized {
		t.Fatalf("State 直接 GET 应 Uninitialized: got (%v, %v)", p, err)
	}

	// 第二次失败：failn INCR → 2，TTL=backoff(2)（min(initial×2^(n-1),max)）
	calls.Store(0)
	if err := co.run(ctx, 0); err == nil {
		t.Fatal("第二次失败应返回错误")
	}
	if v := mustGet(t, mr, co.failnKey); v != "2" {
		t.Fatalf("第二次失败 failn 应为 2，got %q", v)
	}
	if ttl := mr.TTL(co.stateKey); ttl <= b1 || ttl > b2 {
		t.Fatalf("fail TTL 应为 backoff(2)=%v，got %v", b2, ttl)
	}

	// T4: force=1 穿透退避窗口 + DEL failn 归零 → 成功 → ready、failn 清除
	failing.Store(false)
	calls.Store(0)
	if err := pf.Reset(ctx); err != nil {
		t.Fatalf("T4 force=1 穿透: %v", err)
	}
	if v := mustGet(t, mr, co.stateKey); v != pvReady {
		t.Fatalf("T4 完成后应 ready，got %q", v)
	}
	if mr.Exists(co.failnKey) {
		t.Fatal("T4 成功后 failn 应归零（force=1 DEL + ready DEL）")
	}
	if calls.Load() != 1 {
		t.Fatalf("T4 fn 应执行一次，calls=%d", calls.Load())
	}

	// T2 补充: force=1 可从 Ready 抢占重建
	calls.Store(0)
	if err := pf.Reset(ctx); err != nil {
		t.Fatalf("force=1 从 ready 抢占: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("force=1 应从 ready 抢占并执行 fn，calls=%d", calls.Load())
	}
	if v := mustGet(t, mr, co.stateKey); v != pvReady {
		t.Fatalf("抢占重建后应回 ready，got %q", v)
	}

	// T8: Building 执行者崩溃（不写任何状态）→ building TTL 过期 → 键缺失
	if err := rc.Set(ctx, co.stateKey, pvBuilding, 100*time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(200 * time.Millisecond)
	if mr.Exists(co.stateKey) {
		t.Fatal("building TTL 过期后状态键应消失（崩溃自愈）")
	}
	if p, err := pf.State(ctx); err != nil || p != PrefillUninitialized {
		t.Fatalf("T8 过期后 State 应 Uninitialized: got (%v, %v)", p, err)
	}
}

// --- R7: ticker 自动重建链（syncOnce→triggerLazy→run(force=0)） ---

// TestBloomPrefillTickerAutoRebuild 验证后台同步链的自动重建：短
// syncInterval、不关 loop、无人工 Reset，状态须在限期内自动到达
// ready 且 fn 至少被调一次——覆盖 jitter→tick→syncOnce→triggerLazy→
// run(force=0)→Δ→Reset→fn→prefillReady 全链（既有 helper 均 Close
// 掉 loop，本用例专补该链判别力）。
func TestBloomPrefillTickerAutoRebuild(t *testing.T) {
	rc, mr := newMiniRedisClient(t)
	t.Cleanup(func() { _ = rc.Close() })
	ctx := t.Context()
	key := bloomTestKey("pf-tick")

	var calls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		calls.Add(1)
		_, err := ingest.AddMulti(ctx, "tick-seed")
		return err
	}
	f, err := rc.NewBloomFilter(ctx, key,
		WithCapacity(10_000), WithFalsePositive(0.0001),
		WithPrefill(fn, WithSyncInterval(20*time.Millisecond), WithRebuildTimeout(2*time.Second)))
	if err != nil {
		t.Fatalf("NewBloomFilter: %v", err)
	}
	pf, ok := f.(*prefillFilter)
	if !ok {
		t.Fatalf("应为 *prefillFilter，got %T", f)
	}
	// 不调 Close：保留后台 loop 自动驱动；用后清理。
	t.Cleanup(func() { _ = pf.Close() })

	// jitter(<20ms)+首 tick(≤40ms)+Δ(40ms)+fn → 5s 上限足够；超时即链路断裂。
	waitPrefillState(t, mr, pf.coord.stateKey, pvReady, 5*time.Second)
	if n := calls.Load(); n < 1 {
		t.Fatalf("自动重建应至少执行 fn 一次，calls=%d", n)
	}
	// C6：本地相位用 eventually（权威 ready 与 storeLocal 间有固有窗口）。
	waitLocalPhase(t, pf.coord, PrefillReady, time.Second)
}

// --- R2: triggerLazy 相位短路与 Failed 冷却 ---

// TestBloomPrefillTriggerShortCircuit 验证热路径触发按本地相位短路：
// Building 相位不发 acquire（不反向加压）；Failed 且本地冷却未到期不发
// acquire。以 coordinator.acquireTries 为判别探针。
func TestBloomPrefillTriggerShortCircuit(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("pf-short")
	var calls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		calls.Add(1)
		return nil
	}
	pf, rc, mr := newPrefillFilterForTest(t, key, fn,
		WithSyncInterval(20*time.Millisecond),
		WithRetryBackoff(100*time.Millisecond, time.Second))
	co := pf.coord

	// Building：权威与本地均为 Building → 热路径不得发起 acquire。
	if err := rc.Set(ctx, co.stateKey, pvBuilding, 30*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	co.storeLocal(PrefillBuilding)
	base := co.acquireTries.Load()
	for i := range 5 {
		if ok, err := pf.Exists(ctx, fmt.Sprintf("sc-b-%d", i)); err != nil || !ok {
			t.Fatalf("Building 期 Exists 应降级 true: ok=%v err=%v", ok, err)
		}
	}
	time.Sleep(50 * time.Millisecond) // 若有异步 run 已起，给足发出 acquire 的窗口
	if got := co.acquireTries.Load(); got != base {
		t.Fatalf("Building 相位热路径不应发起 acquire：base=%d got=%d", base, got)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("Building 相位不应执行 fn，calls=%d", n)
	}
	if v := mustGet(t, mr, co.stateKey); v != pvBuilding {
		t.Fatalf("状态应保持 building，got %q", v)
	}

	// Failed 且本地冷却未到期 → 热路径不得发起 acquire。
	co.storeLocal(PrefillFailed)
	co.nextTriggerAt.Store(time.Now().Add(time.Hour).UnixNano())
	base2 := co.acquireTries.Load()
	for i := range 5 {
		if ok, err := pf.Exists(ctx, fmt.Sprintf("sc-f-%d", i)); err != nil || !ok {
			t.Fatalf("Failed 期 Exists 应降级 true: ok=%v err=%v", ok, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := co.acquireTries.Load(); got != base2 {
		t.Fatalf("Failed 冷却期热路径不应发起 acquire：base=%d got=%d", base2, got)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("Failed 冷却期不应执行 fn，calls=%d", n)
	}

	// Ready-stale（权威 Ready、本地快照超龄）→ 触发权交还下轮 syncOnce
	// 权威刷新，热路径不得对权威 Ready 发必然被拒的空转 acquire（N4）。
	if err := rc.Set(ctx, co.stateKey, pvReady, 30*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	co.phase.Store(&prefillLocal{phase: PrefillReady, updatedAt: time.Now().Add(-5 * time.Second)})
	base3 := co.acquireTries.Load()
	for i := range 5 {
		if ok, err := pf.Exists(ctx, fmt.Sprintf("sc-r-%d", i)); err != nil || !ok {
			t.Fatalf("Ready-stale 应按非 Ready 降级 true: ok=%v err=%v", ok, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := co.acquireTries.Load(); got != base3 {
		t.Fatalf("Ready-stale 热路径不应发起 acquire（应交还 syncOnce）：base=%d got=%d", base3, got)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("Ready-stale 不应执行 fn，calls=%d", n)
	}

	// N3：Uninitialized 无视 nextTriggerAt 冷却——即便预设未来冷却时刻，
	// 热路径必须放行 acquire 并触发真实重建（权威键缺失 → 抢占 → Δ → fn）。
	mr.Del(co.stateKey) // miniredis Del(key) 返回 bool
	co.phase.Store(&prefillLocal{phase: PrefillUninitialized, updatedAt: time.Now()})
	co.nextTriggerAt.Store(time.Now().Add(time.Hour).UnixNano())
	base4 := co.acquireTries.Load()
	callsBefore := calls.Load()
	for i := range 5 {
		if ok, err := pf.Exists(ctx, fmt.Sprintf("sc-u-%d", i)); err != nil || !ok {
			t.Fatalf("Uninitialized 期 Exists 应降级 true: ok=%v err=%v", ok, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := co.acquireTries.Load(); got <= base4 {
		t.Fatalf("Uninitialized 应无视冷却放行 acquire：base=%d got=%d", base4, got)
	}
	// 真实重建链路完整收口：轮询等待 fn 执行且权威状态到达 ready
	// （Δ=2×20ms + 清空 + fn，2s 上限足够）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if calls.Load() > callsBefore && mustGet(t, mr, co.stateKey) == pvReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Uninitialized 放行后重建未完成：calls=%d want>%d state=%q",
				calls.Load(), callsBefore, mustGet(t, mr, co.stateKey))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- R3: building TTL 裕量随 syncInterval 放大 ---

// TestBloomPrefillBuildingTTL 验证 buildingTTL = rebuildTimeout +
// max(10s, 2×syncInterval+5s)：大 syncInterval（Δ 吃穿固定裕量）时
// TTL 必须覆盖 rebuildTimeout+Δ，否则双执行者窗口。
func TestBloomPrefillBuildingTTL(t *testing.T) {
	// 默认档：2×1s+5s=7s < 10s → 用固定裕量 10s。
	if got := prefillBuildingTTL(5*time.Minute, time.Second); got != 5*time.Minute+10*time.Second {
		t.Fatalf("默认档 buildingTTL: got %v", got)
	}
	// 大 syncInterval 档：sync=5s → Δ=10s → 裕量 max(10s, 15s)=15s。
	rebuild, syncI := time.Second, 5*time.Second
	got := prefillBuildingTTL(rebuild, syncI)
	if want := rebuild + 2*syncI + 5*time.Second; got != want {
		t.Fatalf("大 syncInterval 档 buildingTTL: got %v want %v", got, want)
	}
	// 硬断言（评审口径）：building TTL > rebuildTimeout + Δ。
	if got <= rebuild+2*syncI {
		t.Fatalf("building TTL 未覆盖 rebuildTimeout+Δ：ttl=%v rebuild=%v Δ=%v", got, rebuild, 2*syncI)
	}
}

// TestBloomPrefillBuildingTTLSaturate 验证 N5：病态极大 rebuildTimeout
// （接近 time.Duration 上限）下 rebuildTimeout+margin 加法不得回绕为负
// （负 PX 会让 acquire Lua 报错）——须饱和钳位到 Duration 上限。
func TestBloomPrefillBuildingTTLSaturate(t *testing.T) {
	const maxDur = time.Duration(1<<63 - 1)
	got := prefillBuildingTTL(maxDur-1, time.Second)
	if got <= 0 {
		t.Fatalf("病态极大值加法回绕为负（未饱和）：got=%d", int64(got))
	}
	if want := maxDur; got != want {
		t.Fatalf("应饱和钳位到 Duration 上限：got=%d want=%d", int64(got), int64(want))
	}
}
