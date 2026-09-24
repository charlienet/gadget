package redis

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
)

// Bloom prefill 测试 B：Reset 拦截、Δ 传播等待、stale 降级、panic→fail、
// prefillReady 条件转移（反向验证目标）、closeState 级联、双实例互斥
// （真 Redis gate，规格 §8-5..§8-10）。

// --- 5) Reset 拦截（规格 §8-5） ---

// TestBloomPrefillResetIntercept 验证启用后 Reset 等价 force 重建
// 全流程：抢占 → Δ → 清空 → fn 回灌 → ready；fn 被调用、状态到 ready。
func TestBloomPrefillResetIntercept(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("pf-rst")
	var calls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		calls.Add(1)
		_, err := ingest.AddMulti(ctx, "rst-seed")
		return err
	}
	pf, _, mr := newPrefillFilterForTest(t, key, fn, WithSyncInterval(20*time.Millisecond), WithRebuildTimeout(time.Second))

	// 先写入数据（经 inner 直写，避免热路径惰性重建抢先持锁干扰断言）
	if _, err := pf.inner.Add(ctx, "pre-1"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Reset 拦截 = force 重建 全流程
	if err := pf.Reset(ctx); err != nil {
		t.Fatalf("Reset（=force 重建）: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("Reset 应经 prefill 流程调用 fn 一次，calls=%d", calls.Load())
	}
	if v := mustGet(t, mr, pf.coord.stateKey); v != pvReady {
		t.Fatalf("Reset 完成后状态应 ready，got %q", v)
	}
	// fn 回灌生效（Ready 真实查询）
	if ok, err := pf.Exists(ctx, "rst-seed"); err != nil || !ok {
		t.Fatalf("Reset 后回灌项应命中: ok=%v err=%v", ok, err)
	}
}

// --- 6) Δ 传播等待（规格 §8-6） ---

// TestBloomPrefillDeltaWait 验证 acquire 成功后不立即清空、Δ=2×interval
// 后才 Reset：观察到 building 时数据键仍在；fn 执行时（Reset 后）位图已
// 清空；Reset 总耗时 ≥ Δ。
func TestBloomPrefillDeltaWait(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("pf-delta")
	const syncI = 100 * time.Millisecond // Δ = 200ms

	var mrRef *miniredis.Miniredis
	var clearedAtFn atomic.Bool
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		// fn 在 Reset 之后执行：位图应已被清空（全零）
		if mrRef != nil {
			if v, err := mrRef.Get(key); err == nil {
				allZero := true
				for i := 0; i < len(v); i++ {
					if v[i] != 0 {
						allZero = false
						break
					}
				}
				clearedAtFn.Store(allZero)
			}
		}
		_, err := ingest.AddMulti(ctx, "delta-seed")
		return err
	}
	pf, _, mr := newPrefillFilterForTest(t, key, fn,
		WithSyncInterval(syncI), WithRebuildTimeout(2*time.Second))
	mrRef = mr
	co := pf.coord

	// 预写数据：位置图非零
	if _, err := pf.inner.Add(ctx, "delta-item"); err != nil {
		t.Fatalf("预写: %v", err)
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- pf.Reset(ctx) }()

	// 等待 acquire 完成（状态进入 building）
	waitPrefillState(t, mr, co.stateKey, pvBuilding, time.Second)
	if !mr.Exists(key) {
		t.Fatal("Δ 窗口内数据键不应被清空（acquire 后立即 Reset 违规）")
	}
	if v, err := mr.Get(key); err == nil {
		nonzero := false
		for i := 0; i < len(v); i++ {
			if v[i] != 0 {
				nonzero = true
				break
			}
		}
		if !nonzero {
			t.Fatal("Δ 窗口内位图不应已被清空")
		}
	}

	if err := <-done; err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 2*syncI {
		t.Fatalf("Reset 总耗时应 ≥ Δ=2×syncInterval=%v，got %v", 2*syncI, elapsed)
	}
	if !clearedAtFn.Load() {
		t.Fatal("fn 执行时位图应已被 Reset 清空（全零）")
	}
}

// --- 7) stale 降级（规格 §8-7、T10） ---

// TestBloomPrefillStaleDegrade 验证本地缓存超龄（age > 2×syncInterval）
// 后热路径按非 Ready 降级；新鲜 Ready 恢复真实查询。
func TestBloomPrefillStaleDegrade(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("pf-stale")
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		_, err := ingest.AddMulti(ctx, "stale-seed")
		return err
	}
	pf, _, _ := newPrefillFilterForTest(t, key, fn, WithSyncInterval(100*time.Millisecond), WithRebuildTimeout(time.Second))
	co := pf.coord

	// Ready 但 updatedAt 超龄（age=5s > stale=200ms）→ 按非 Ready 降级
	co.phase.Store(&prefillLocal{phase: PrefillReady, updatedAt: time.Now().Add(-5 * time.Second)})
	if ok, err := pf.Exists(ctx, "stale-miss"); err != nil || !ok {
		t.Fatalf("stale Ready 应按非 Ready 降级恒 true: ok=%v err=%v", ok, err)
	}

	// 新鲜 Ready → 真实查询（miss=false；FP=1e-4 且位图近乎空，假阳可忽略）
	co.storeLocal(PrefillReady)
	if s := co.loadLocal(); s.phase != PrefillReady || time.Since(s.updatedAt) > 2*co.cfg.syncInterval {
		t.Fatalf("storeLocal 后应新鲜 Ready，got %+v", s)
	}
	if ok, err := pf.Exists(ctx, "stale-miss"); err != nil || ok {
		t.Fatalf("新鲜 Ready 应真实查询 false: ok=%v err=%v", ok, err)
	}

	// age 阈值边界：updatedAt 恰在 2×interval 内 → 新鲜；超一点 → stale
	co.phase.Store(&prefillLocal{phase: PrefillReady, updatedAt: time.Now().Add(-2*co.cfg.syncInterval + 50*time.Millisecond)})
	if ok, err := pf.Exists(ctx, "stale-miss"); err != nil || ok {
		t.Fatalf("阈值内应新鲜真实查询: ok=%v err=%v", ok, err)
	}
}

// --- 8) panic recover → fail（规格 §8-8） ---

// TestBloomPrefillPanicFail 验证 fn panic 由库 recover 转 error 走 fail
// 路径：Reset 返回错误、状态 fail、TTL=backoff(1)、failn=1、building
// 锁被 fail 覆盖（释放语义）。
func TestBloomPrefillPanicFail(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("pf-panic")
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		panic("prefill fn exploded")
	}
	pf, _, mr := newPrefillFilterForTest(t, key, fn,
		WithSyncInterval(20*time.Millisecond),
		WithRetryBackoff(300*time.Millisecond, 10*time.Second))
	co := pf.coord

	err := pf.Reset(ctx)
	if err == nil {
		t.Fatal("fn panic 应被 recover 转为 error")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("错误应含 panic 信息，got %v", err)
	}
	if v := mustGet(t, mr, co.stateKey); v != pvFail {
		t.Fatalf("panic 后状态应 fail（building 锁释放），got %q", v)
	}
	// R8：区间断言 [300ms-50ms, 300ms]——既防过长也抓过短。
	if ttl := mr.TTL(co.stateKey); ttl < 300*time.Millisecond-50*time.Millisecond || ttl > 300*time.Millisecond {
		t.Fatalf("panic 后 TTL 应落在 backoff(1)=300ms±50ms，got %v", ttl)
	}
	if v := mustGet(t, mr, co.failnKey); v != "1" {
		t.Fatalf("panic 后 failn 应为 1，got %q", v)
	}
}

// --- 9) prefillReady 条件转移（T6；反向验证目标②） ---

// TestBloomPrefillReadyScriptConditional 验证 prefillReady 仅当当前值
// 仍=building 才 SET ready（无 TTL）+DEL failn 返回 1；否则返回 0 且
// 不改状态。反向验证会把条件改无条件 SET，本用例必须变红。
func TestBloomPrefillReadyScriptConditional(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("pf-ready")
	fn := func(ctx context.Context, ingest PrefillIngest) error { return nil }
	pf, rc, mr := newPrefillFilterForTest(t, key, fn, WithSyncInterval(time.Hour))
	co := pf.coord

	// 状态=fail → 返回 0，状态与 failn 均不改
	if err := rc.Set(ctx, co.stateKey, pvFail, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rc.Set(ctx, co.failnKey, "3", 0).Err(); err != nil {
		t.Fatal(err)
	}
	ok, err := co.prefillReady(ctx)
	if err != nil || ok {
		t.Fatalf("非 building 时 prefillReady 应返回 (false,nil)，got (%v,%v)", ok, err)
	}
	if v := mustGet(t, mr, co.stateKey); v != pvFail {
		t.Fatalf("非 building 时状态不得被改写（无条件 SET 即红），got %q", v)
	}
	if v := mustGet(t, mr, co.failnKey); v != "3" {
		t.Fatalf("返回 0 时 failn 不应被删，got %q", v)
	}

	// 键缺失 → 返回 0，仍缺失
	if err := rc.Del(ctx, co.stateKey).Err(); err != nil {
		t.Fatal(err)
	}
	if ok, err := co.prefillReady(ctx); err != nil || ok {
		t.Fatalf("键缺失时应 (false,nil)，got (%v,%v)", ok, err)
	}
	if mr.Exists(co.stateKey) {
		t.Fatal("键缺失时不应被创建")
	}

	// 状态=building → 返回 1，置 ready（无 TTL 持久）+DEL failn
	if err := rc.Set(ctx, co.stateKey, pvBuilding, 5*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rc.Set(ctx, co.failnKey, "7", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if ok, err := co.prefillReady(ctx); err != nil || !ok {
		t.Fatalf("building 时应 (true,nil)，got (%v,%v)", ok, err)
	}
	if v := mustGet(t, mr, co.stateKey); v != pvReady {
		t.Fatalf("building 完成后应 ready，got %q", v)
	}
	if ttl := mr.TTL(co.stateKey); ttl != 0 {
		t.Fatalf("ready 应无 TTL 持久，got %v", ttl)
	}
	if mr.Exists(co.failnKey) {
		t.Fatal("ready 时 failn 应被 DEL")
	}
}

// --- 4) closeState 级联（规格 §4 生命周期） ---

// TestBloomPrefillCloseCascade 验证 coordinator 注册进 closeState：
// GracefulClose 时停 ticker（stopCh 关闭）；重复 Close 幂等。
func TestBloomPrefillCloseCascade(t *testing.T) {
	ctx := t.Context()
	rc, _ := newMiniRedisClient(t)
	fn := func(ctx context.Context, ingest PrefillIngest) error { return nil }
	f, err := rc.NewBloomFilter(ctx, bloomTestKey("pf-cascade"),
		WithCapacity(10_000), WithPrefill(fn, WithSyncInterval(time.Hour)))
	if err != nil {
		t.Fatalf("NewBloomFilter: %v", err)
	}
	pf, ok := f.(*prefillFilter)
	if !ok {
		t.Fatalf("应为 *prefillFilter，got %T", f)
	}

	// 注册进 closeState 级联表
	rc.state.mu.Lock()
	n := len(rc.state.closers)
	rc.state.mu.Unlock()
	if n == 0 {
		t.Fatal("coordinator 应注册进 closeState.closers")
	}

	if err := rc.GracefulClose(ctx); err != nil {
		t.Fatalf("GracefulClose: %v", err)
	}
	select {
	case <-pf.coord.stopCh:
	default:
		t.Fatal("GracefulClose 应停 coordinator ticker（stopCh 关闭）")
	}
	// 幂等
	if err := pf.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	if err := rc.GracefulClose(ctx); err != nil {
		t.Fatalf("重复 GracefulClose: %v", err)
	}
}

// --- 10) 双实例并发互斥（规格 §8-10，真 Redis gate） ---

// TestBloomPrefillDualInstanceMutexRealRedis 在真实 Redis 上验证并发双
// 实例互斥：Lua acquire 原子，任一时刻至多一个实例执行 fn。miniredis 的
// EVAL 非原子（bloom_internal_test.go:319 先例），断言不可靠 → gate 到
// REDIS_URL，无环境 Skip；随机前缀键 + 收尾 Del 清理（严禁 FLUSHDB）。
func TestBloomPrefillDualInstanceMutexRealRedis(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skip real-Redis prefill mutex test")
	}
	rdb, err := NewWithURL(url)
	if err != nil {
		t.Fatalf("NewWithURL: %v", err)
	}
	defer func() { _ = rdb.GracefulClose(context.Background()) }()

	ctx := context.Background()
	key := bloomTestKey("pf-mutex")
	stateKey, failnKey := prefillStateKeys(key)
	defer func() {
		_ = rdb.Del(ctx, key, stateKey, failnKey).Err()
	}()

	var active, maxActive, runs atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		cur := active.Add(1)
		for {
			old := maxActive.Load()
			if cur <= old || maxActive.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(200 * time.Millisecond) // 拉长 building 窗口
		active.Add(-1)
		runs.Add(1)
		_, err := ingest.AddMulti(ctx, "mutex-seed")
		return err
	}

	mk := func() *prefillFilter {
		t.Helper()
		f, err := rdb.NewBloomFilter(ctx, key,
			WithCapacity(10_000), WithFalsePositive(0.0001),
			WithPrefill(fn, WithSyncInterval(50*time.Millisecond), WithRebuildTimeout(5*time.Second)))
		if err != nil {
			t.Fatalf("NewBloomFilter: %v", err)
		}
		pf, ok := f.(*prefillFilter)
		if !ok {
			t.Fatalf("应为 *prefillFilter，got %T", f)
		}
		pf.coord.stopLoop() // 只停各自后台 loop（不 cancel run），仅测显式并发 Reset
		return pf
	}
	pf1, pf2 := mk(), mk()

	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() { <-start; errs <- pf1.Reset(ctx) }()
	go func() { <-start; errs <- pf2.Reset(ctx) }()
	close(start)

	// 收集：胜者在 fn 内 200ms，败者立即返回 ErrRebuildInProgress
	var results []error
	deadline := time.After(5 * time.Second)
	for len(results) < 2 {
		select {
		case e := <-errs:
			results = append(results, e)
		case <-deadline:
			t.Fatalf("等待 Reset 返回超时，已有 %d 个结果", len(results))
		}
	}

	if maxActive.Load() > 1 {
		t.Fatalf("互斥破坏：fn 并发进入峰值 %d > 1", maxActive.Load())
	}
	if runs.Load() == 0 {
		t.Fatal("至少应有一个实例完成 fn")
	}
	for i, e := range results {
		if e != nil && !errors.Is(e, ErrRebuildInProgress) {
			t.Fatalf("results[%d] 意外错误: %v", i, e)
		}
	}
}

// --- R5: Close 取消在飞 run ---

// TestBloomPrefillCloseCancelsInflightRun 验证完整 Close 不仅停 ticker，
// 还取消在飞 run：fn 阻塞等 ctx 取消 → Close → Reset 返回取消错误且
// 状态仍 building（Canceled 禁写分支覆盖，不写 ready/fail）。
func TestBloomPrefillCloseCancelsInflightRun(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("pf-close")
	fnStarted := make(chan struct{})
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		close(fnStarted)
		<-ctx.Done() // 阻塞至 Close 取消（预算 RebuildTimeout 兜底 5s）
		return ctx.Err()
	}
	pf, _, mr := newPrefillFilterForTest(t, key, fn,
		WithSyncInterval(20*time.Millisecond), WithRebuildTimeout(5*time.Second))
	co := pf.coord

	done := make(chan error, 1)
	go func() { done <- pf.Reset(ctx) }()
	select {
	case <-fnStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("fn 未在限期内启动")
	}

	if err := pf.Close(); err != nil { // 完整 Close：停 loop + cancel 在飞 run
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Close 取消在飞 run 应返回 context.Canceled，got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close 后 Reset 未返回（在飞 run 未被取消）")
	}
	// Canceled 禁写：状态停留 building，未被写成 ready/fail。
	if v := mustGet(t, mr, co.stateKey); v != pvBuilding {
		t.Fatalf("取消后状态应停留 building（禁写），got %q", v)
	}
}
