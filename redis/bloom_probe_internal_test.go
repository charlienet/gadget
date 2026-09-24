package redis

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
)

// Bloom/布谷鸟 有效性探测（liveness probe）测试：probeLoop 按间隔取
// 肯定样本直查 inner，任一 miss（bloom 无假阴 = 确定性丢数据证据）触发
// force=1 重建；错误/panic/空样本/非 Ready/未启用一律跳过（错误≠失效）。
//
// 与测试 A/B 的差异：被测对象就是后台 probeLoop 本身，故 helper 不
// stopLoop（loop 与 probeLoop 并存）；权威状态先于构造 SET（消除 loop
// 首拍 triggerLazy 冷启动竞态），本地相位经 storeLocal 手动控制。
// PrefillOption 为 bloom/cuckoo 共用，选项形态两边一致（cuckoo 经
// WithCuckooPrefill 传入同款，接线见 cuckoo.go mkCoord——共用同一
// newPrefillCoordinator，本文件以 bloom 侧为载体）。

// newPrefillProbeForTest 构造启用 prefill（可带 WithLivenessProbe）的
// 过滤器：seedState 非空时先 SET 权威状态键（先于 coordinator 启动——
// loop 首拍即见该状态，ready 不触发 triggerLazy 竞态）；构造后按
// seedState 同步本地相位。不 stopLoop：probeLoop 是被测对象，须与
// loop 并存运行；数据面不被调用，loop 的 syncOnce 仅刷新相位（权威
// building/ready 均不触发 force=0 重建）。
func newPrefillProbeForTest(t *testing.T, key, seedState string, fn PrefillFunc, opts ...PrefillOption) (*prefillFilter, *redisClient, *miniredis.Miniredis) {
	t.Helper()
	rc, mr := newMiniRedisClient(t)
	ctx := t.Context()
	stateKey, _ := prefillStateKeys(key)
	if seedState != "" {
		if err := rc.Set(ctx, stateKey, seedState, 0).Err(); err != nil {
			t.Fatalf("seed state: %v", err)
		}
	}
	f, err := rc.NewBloomFilter(ctx, key,
		WithCapacity(10_000),
		WithFalsePositive(0.0001),
		WithPrefill(fn, opts...),
	)
	if err != nil {
		t.Fatalf("NewBloomFilter: %v", err)
	}
	pf, ok := f.(*prefillFilter)
	if !ok {
		t.Fatalf("启用 WithPrefill 后应返回 *prefillFilter，got %T", f)
	}
	switch seedState {
	case pvReady:
		pf.coord.storeLocal(PrefillReady)
	case pvBuilding:
		pf.coord.storeLocal(PrefillBuilding)
	}
	t.Cleanup(func() {
		waitInflightDone(pf.coord, 3*time.Second)
		_ = pf.Close()
	})
	return pf, rc, mr
}

// newProbeCoordinatorForTest 白盒直接组装启用探测的协调器（仅供需要
// 定制 inner 桩的用例）：不经工厂 NewBloomFilter→newPrefillFilter——
// 工厂路径下 coordinator 构造即 `go probeLoop`，测试再运行期替换
// co.inner 字段属非原子写，与 probeLoop 的 c.inner 读构成 DATA RACE。
// 本 helper 让桩在 newPrefillCoordinator 构造实参时就位（inner 字段
// 出生即定格，构造后无任何写），从根上消除竞态面。
//
// wrap 非 nil 时包装真实 inner（真实 inner 取不带 WithPrefill 的
// NewBloomFilter 返回值——该路径不创建 coordinator，无竞态）；seedState
// 处理、storeLocal 相位同步（原子路径）、loop 与 probeLoop 并存不
// stopLoop、t.Cleanup 关 coordinator 等约定同 newPrefillProbeForTest。
func newProbeCoordinatorForTest(t *testing.T, key, seedState string, fn PrefillFunc,
	wrap func(real prefillInner) prefillInner, opts ...PrefillOption,
) (*prefillCoordinator, *miniredis.Miniredis) {
	t.Helper()
	rc, mr := newMiniRedisClient(t)
	ctx := t.Context()
	stateKey, _ := prefillStateKeys(key)
	if seedState != "" {
		if err := rc.Set(ctx, stateKey, seedState, 0).Err(); err != nil {
			t.Fatalf("seed state: %v", err)
		}
	}
	// 不带 WithPrefill：返回裸 BloomFilter（满足 prefillInner），不创建
	// coordinator——仅作为桩的内层真实实现（本用例数据面不被调用）。
	real, err := rc.NewBloomFilter(ctx, key,
		WithCapacity(10_000),
		WithFalsePositive(0.0001),
	)
	if err != nil {
		t.Fatalf("NewBloomFilter: %v", err)
	}
	var inner prefillInner = real
	if wrap != nil {
		inner = wrap(real)
	}
	// 对齐 WithPrefill 的字段写入形态：enabled/fn 直写，其余经 opts 平铺
	//（probeInterval/probeFn 由 WithLivenessProbe 写入 cfg）。
	cfg := defaultPrefillConfig()
	cfg.enabled = true
	cfg.fn = fn
	for _, o := range opts {
		o(&cfg)
	}
	co := newPrefillCoordinator(rc, inner, key, cfg)
	switch seedState {
	case pvReady:
		co.storeLocal(PrefillReady)
	case pvBuilding:
		co.storeLocal(PrefillBuilding)
	}
	t.Cleanup(func() {
		waitInflightDone(co, 3*time.Second)
		co.Close()
	})
	return co, mr
}

// waitUntilCond 轮询等待条件成立（5ms 一拍）；超时 Fatal 并打印诊断
// （diag 惰性求值：仅超时才构造，避免热路径里反复读状态）。
func waitUntilCond(t *testing.T, timeout time.Duration, diag func() string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时（%v）：%s", timeout, diag())
}

// --- 1) 探测健康：Ready + 样本全命中 → 不触发重建 ---

// TestBloomProbeHealthyNoRebuild 验证健康路径：本地新鲜 Ready、probeFn
// 返回肯定样本、inner.ExistsMulti 全命中 → 不触发重建（acquireTries/
// 回灌 fn 计数不变、权威与本地相位保持 ready）。probeCalls≥1 证明探测
// 确实发生（排除"没探测所以没触发"的假绿）。
func TestBloomProbeHealthyNoRebuild(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("probe-healthy")
	const probeI = 80 * time.Millisecond

	var fnCalls, probeCalls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		fnCalls.Add(1)
		_, err := ingest.AddMulti(ctx, "probe-seed")
		return err
	}
	liveness := func(ctx context.Context) ([]any, error) {
		probeCalls.Add(1)
		return []any{"probe-seed"}, nil
	}
	pf, _, mr := newPrefillProbeForTest(t, key, pvReady, fn,
		WithSyncInterval(50*time.Millisecond),
		WithRebuildTimeout(2*time.Second),
		WithLivenessProbe(probeI, liveness))
	co := pf.coord

	// 样本入 inner（直写不经装饰器，不触发惰性重建）
	if _, err := pf.inner.Add(ctx, "probe-seed"); err != nil {
		t.Fatalf("Add seed: %v", err)
	}
	co.storeLocal(PrefillReady)

	baseTries := co.acquireTries.Load()
	baseFn := fnCalls.Load()

	// 观察 6 拍：首拍 jitter<probeI + 每 probeI 一拍 → 至少数次探测
	time.Sleep(6 * probeI)

	if n := probeCalls.Load(); n < 1 {
		t.Fatalf("观察窗口内 probeFn 应至少被调一次（探测确实发生），probeCalls=%d", n)
	}
	if got := co.acquireTries.Load(); got != baseTries {
		t.Fatalf("健康探测不应触发重建（acquire 不变）：base=%d got=%d", baseTries, got)
	}
	if got := fnCalls.Load(); got != baseFn {
		t.Fatalf("健康探测不应触发回灌 fn：base=%d got=%d", baseFn, got)
	}
	if v := mustGet(t, mr, co.stateKey); v != pvReady {
		t.Fatalf("健康探测后权威状态应保持 ready，got %q", v)
	}
	if s := co.loadLocal(); s.phase != PrefillReady {
		t.Fatalf("健康探测后本地相位应保持 Ready，got %v", s.phase)
	}
}

// --- 2) 探测失效：样本查不到 → run(force=1) → 回灌收口 ready ---

// TestBloomProbeFailureTriggersForceRebuild 验证失效路径：权威/本地均为
// Ready 但肯定样本从未回灌（ExistsMulti=false = 数据已丢的确定性证据）
// → probeLoop 触发 run(force=1)（与 Reset 同路）→ 回灌 fn 被调 →
// 最终权威 ready、样本经回灌可命中收口。
// run 预算为 RebuildTimeout(2s)（独立于探测 ctx，见方案 A）：
// Δ=2×20ms=40ms + Reset + fn ≈ 100ms < 2s。
func TestBloomProbeFailureTriggersForceRebuild(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("probe-fail")
	const probeI = time.Second

	var fnCalls, probeCalls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		fnCalls.Add(1)
		_, err := ingest.AddMulti(ctx, "probe-seed")
		return err
	}
	liveness := func(ctx context.Context) ([]any, error) {
		probeCalls.Add(1)
		// 肯定样本：数据源确定存在，但过滤器从未 Add → inner 必 miss
		return []any{"probe-seed"}, nil
	}
	pf, _, mr := newPrefillProbeForTest(t, key, pvReady, fn,
		WithSyncInterval(20*time.Millisecond),
		WithRebuildTimeout(2*time.Second),
		WithLivenessProbe(probeI, liveness))
	co := pf.coord
	co.storeLocal(PrefillReady)
	// 故意不 Add "probe-seed"：Ready 态但样本查不到

	// 等待探测触发 force 重建完成（首拍 jitter<1s + run≈100ms；5s 上限）
	waitUntilCond(t, 5*time.Second, func() string {
		return "失效探测未触发重建：probeCalls=" + strconv.Itoa(int(probeCalls.Load())) +
			" fnCalls=" + strconv.Itoa(int(fnCalls.Load())) +
			" state=" + mustGet(t, mr, co.stateKey)
	}, func() bool {
		return fnCalls.Load() >= 1 && mustGet(t, mr, co.stateKey) == pvReady
	})
	if probeCalls.Load() < 1 {
		t.Fatalf("probeFn 应至少被调一次，probeCalls=%d", probeCalls.Load())
	}
	// 同轮不重查：收口采样时刻 ≈ 首拍 jitter + run(≈100ms)，而下一轮
	// 探测拍 = jitter + probeI(1s) 恒落在收口后 ~900ms——此刻 probeCalls
	// 若 >1 只能来自"发现失效的同一拍内重查"（单轮触发亦锚定 fnCalls=1）。
	if n := probeCalls.Load(); n > 1 {
		t.Fatalf("发现失效到重建完成的同轮窗口内不应重查：probeCalls=%d", n)
	}
	if n := fnCalls.Load(); n != 1 {
		t.Fatalf("单轮探测应只触发一次重建：fnCalls=%d", n)
	}

	// 收口：重建后回灌生效，样本可命中；本地相位回 Ready
	waitLocalPhase(t, co, PrefillReady, 2*time.Second)
	if ok, err := pf.Exists(ctx, "probe-seed"); err != nil || !ok {
		t.Fatalf("重建后样本应命中（回灌收口）: ok=%v err=%v", ok, err)
	}
}

// --- 3) 边界：空样本 / probeFn error / panic / 非 Ready / stale / 未启用 ---

// TestBloomProbeSkipCases 覆盖一切"本轮结束、不构成失效"的分支：
// 空样本、probeFn 返回 error、probeFn panic（不崩进程）、非 Ready 相位
// （不取样不 acquire）、interval<=0 或 fn==nil 不启 probeLoop（探测不发生）。
func TestBloomProbeSkipCases(t *testing.T) {
	const probeI = 60 * time.Millisecond
	// 观察 5 拍：首拍 jitter<probeI + 4 拍 → 启用探测的 case 至少取样一次
	const window = 5 * probeI

	t.Run("空样本跳过", func(t *testing.T) {
		var probeCalls, fnCalls atomic.Int32
		fn := func(ctx context.Context, ingest PrefillIngest) error {
			fnCalls.Add(1)
			return nil
		}
		liveness := func(ctx context.Context) ([]any, error) {
			probeCalls.Add(1)
			return []any{}, nil // 空样本：本轮结束，不构成失效
		}
		pf, _, mr := newPrefillProbeForTest(t, bloomTestKey("probe-empty"), pvReady, fn,
			WithSyncInterval(50*time.Millisecond),
			WithRebuildTimeout(2*time.Second),
			WithLivenessProbe(probeI, liveness))
		co := pf.coord
		co.storeLocal(PrefillReady)
		base := co.acquireTries.Load()

		time.Sleep(window)
		if probeCalls.Load() < 1 {
			t.Fatalf("probeFn 应被调用，probeCalls=%d", probeCalls.Load())
		}
		if got := co.acquireTries.Load(); got != base {
			t.Fatalf("空样本不应触发重建：base=%d got=%d", base, got)
		}
		if fnCalls.Load() != 0 {
			t.Fatalf("空样本不应触发回灌 fn，fnCalls=%d", fnCalls.Load())
		}
		if v := mustGet(t, mr, co.stateKey); v != pvReady {
			t.Fatalf("状态应保持 ready，got %q", v)
		}
	})

	t.Run("probeFn error 跳过", func(t *testing.T) {
		var probeCalls, fnCalls atomic.Int32
		fn := func(ctx context.Context, ingest PrefillIngest) error {
			fnCalls.Add(1)
			return nil
		}
		liveness := func(ctx context.Context) ([]any, error) {
			probeCalls.Add(1)
			return nil, errors.New("data source down") // 错误≠失效：跳过本轮
		}
		pf, _, mr := newPrefillProbeForTest(t, bloomTestKey("probe-err"), pvReady, fn,
			WithSyncInterval(50*time.Millisecond),
			WithRebuildTimeout(2*time.Second),
			WithLivenessProbe(probeI, liveness))
		co := pf.coord
		co.storeLocal(PrefillReady)
		base := co.acquireTries.Load()

		time.Sleep(window)
		if probeCalls.Load() < 1 {
			t.Fatalf("probeFn 应被调用，probeCalls=%d", probeCalls.Load())
		}
		if got := co.acquireTries.Load(); got != base {
			t.Fatalf("probeFn error 不应触发重建：base=%d got=%d", base, got)
		}
		if fnCalls.Load() != 0 {
			t.Fatalf("probeFn error 不应触发回灌 fn，fnCalls=%d", fnCalls.Load())
		}
		if v := mustGet(t, mr, co.stateKey); v != pvReady {
			t.Fatalf("状态应保持 ready，got %q", v)
		}
	})

	t.Run("probeFn panic 跳过不崩", func(t *testing.T) {
		var probeCalls, fnCalls atomic.Int32
		fn := func(ctx context.Context, ingest PrefillIngest) error {
			fnCalls.Add(1)
			return nil
		}
		liveness := func(ctx context.Context) ([]any, error) {
			probeCalls.Add(1)
			panic("liveness exploded") // 应被 recover：不崩进程、跳过本轮
		}
		pf, _, mr := newPrefillProbeForTest(t, bloomTestKey("probe-panic"), pvReady, fn,
			WithSyncInterval(50*time.Millisecond),
			WithRebuildTimeout(2*time.Second),
			WithLivenessProbe(probeI, liveness))
		co := pf.coord
		co.storeLocal(PrefillReady)
		base := co.acquireTries.Load()

		time.Sleep(window) // 若 panic 未被 recover，goroutine panic 会崩整个测试进程
		if probeCalls.Load() < 1 {
			t.Fatalf("probeFn 应被调用，probeCalls=%d", probeCalls.Load())
		}
		if got := co.acquireTries.Load(); got != base {
			t.Fatalf("probeFn panic 不应触发重建：base=%d got=%d", base, got)
		}
		if fnCalls.Load() != 0 {
			t.Fatalf("probeFn panic 不应触发回灌 fn，fnCalls=%d", fnCalls.Load())
		}
		if v := mustGet(t, mr, co.stateKey); v != pvReady {
			t.Fatalf("状态应保持 ready，got %q", v)
		}
	})

	t.Run("非 Ready 相位不探测", func(t *testing.T) {
		var probeCalls, fnCalls atomic.Int32
		fn := func(ctx context.Context, ingest PrefillIngest) error {
			fnCalls.Add(1)
			return nil
		}
		liveness := func(ctx context.Context) ([]any, error) {
			probeCalls.Add(1)
			return []any{"never"}, nil
		}
		// 权威+本地均 Building：降级期无判别意义，且避免重建期误触发
		pf, _, mr := newPrefillProbeForTest(t, bloomTestKey("probe-building"), pvBuilding, fn,
			WithSyncInterval(50*time.Millisecond),
			WithRebuildTimeout(2*time.Second),
			WithLivenessProbe(probeI, liveness))
		co := pf.coord
		co.storeLocal(PrefillBuilding)
		base := co.acquireTries.Load()

		time.Sleep(window)
		if probeCalls.Load() != 0 {
			t.Fatalf("非 Ready 相位不应取样，probeCalls=%d", probeCalls.Load())
		}
		if got := co.acquireTries.Load(); got != base {
			t.Fatalf("非 Ready 相位不应发起 acquire：base=%d got=%d", base, got)
		}
		if fnCalls.Load() != 0 {
			t.Fatalf("非 Ready 相位不应执行回灌 fn，fnCalls=%d", fnCalls.Load())
		}
		if v := mustGet(t, mr, co.stateKey); v != pvBuilding {
			t.Fatalf("状态应保持 building，got %q", v)
		}
	})

	t.Run("Ready 但 stale 不探测", func(t *testing.T) {
		var probeCalls, fnCalls atomic.Int32
		fn := func(ctx context.Context, ingest PrefillIngest) error {
			fnCalls.Add(1)
			return nil
		}
		liveness := func(ctx context.Context) ([]any, error) {
			probeCalls.Add(1)
			return []any{"never"}, nil
		}
		pf, _, mr := newPrefillProbeForTest(t, bloomTestKey("probe-stale"), pvReady, fn,
			WithSyncInterval(50*time.Millisecond),
			WithRebuildTimeout(2*time.Second),
			WithLivenessProbe(probeI, liveness))
		co := pf.coord
		// 构造 updatedAt 超龄的 Ready 快照（age=10s > 2×syncInterval=100ms），
		// 紧邻调用 probeOnce 消除 loop 首拍刷新的竞态窗口——readyFresh 的
		// isStale 分支在探测场景必须按非 Ready 跳过（行为锚）。
		co.phase.Store(&prefillLocal{phase: PrefillReady, updatedAt: time.Now().Add(-10 * time.Second)})
		base := co.acquireTries.Load()

		co.probeOnce(probeI)
		if probeCalls.Load() != 0 {
			t.Fatalf("Ready 但 stale 不应探测取样，probeCalls=%d", probeCalls.Load())
		}
		if got := co.acquireTries.Load(); got != base {
			t.Fatalf("stale 不应发起 acquire：base=%d got=%d", base, got)
		}
		if fnCalls.Load() != 0 {
			t.Fatalf("stale 不应执行回灌 fn，fnCalls=%d", fnCalls.Load())
		}
		if v := mustGet(t, mr, co.stateKey); v != pvReady {
			t.Fatalf("状态应保持 ready，got %q", v)
		}
	})

	t.Run("interval<=0 不启 probeLoop", func(t *testing.T) {
		var probeCalls, fnCalls atomic.Int32
		fn := func(ctx context.Context, ingest PrefillIngest) error {
			fnCalls.Add(1)
			return nil
		}
		liveness := func(ctx context.Context) ([]any, error) {
			probeCalls.Add(1)
			return []any{"x"}, nil
		}
		// interval=0 → 选项静默忽略（保留零值）→ probeLoop 直接 return
		// （若误启动：Int64N(0)/Ticker(0) 会 panic 崩进程，同样判红）
		pf, _, mr := newPrefillProbeForTest(t, bloomTestKey("probe-iv0"), pvReady, fn,
			WithSyncInterval(50*time.Millisecond),
			WithRebuildTimeout(2*time.Second),
			WithLivenessProbe(0, liveness))
		co := pf.coord
		co.storeLocal(PrefillReady)
		base := co.acquireTries.Load()

		time.Sleep(window)
		if probeCalls.Load() != 0 {
			t.Fatalf("interval<=0 不应启动探测（无空转），probeCalls=%d", probeCalls.Load())
		}
		if got := co.acquireTries.Load(); got != base {
			t.Fatalf("不应触发重建：base=%d got=%d", base, got)
		}
		if fnCalls.Load() != 0 {
			t.Fatalf("不应执行回灌 fn，fnCalls=%d", fnCalls.Load())
		}
		if v := mustGet(t, mr, co.stateKey); v != pvReady {
			t.Fatalf("状态应保持 ready，got %q", v)
		}
	})

	t.Run("fn nil 不启 probeLoop", func(t *testing.T) {
		var fnCalls atomic.Int32
		fn := func(ctx context.Context, ingest PrefillIngest) error {
			fnCalls.Add(1)
			return nil
		}
		// fn=nil → 选项静默忽略 → probeLoop 直接 return（若误启动会
		// nil 调用 panic 崩进程，同样判红）；断言无探测副作用。
		pf, _, mr := newPrefillProbeForTest(t, bloomTestKey("probe-fnnil"), pvReady, fn,
			WithSyncInterval(50*time.Millisecond),
			WithRebuildTimeout(2*time.Second),
			WithLivenessProbe(probeI, nil))
		co := pf.coord
		co.storeLocal(PrefillReady)
		base := co.acquireTries.Load()

		time.Sleep(window)
		if got := co.acquireTries.Load(); got != base {
			t.Fatalf("fn nil 不应触发重建：base=%d got=%d", base, got)
		}
		if fnCalls.Load() != 0 {
			t.Fatalf("fn nil 不应执行回灌 fn，fnCalls=%d", fnCalls.Load())
		}
		if v := mustGet(t, mr, co.stateKey); v != pvReady {
			t.Fatalf("状态应保持 ready，got %q", v)
		}
	})
}

// --- 4) ExistsMulti 错误路径：错误≠失效，本轮跳过 ---

// TestBloomProbeExistsMultiErrorSkips 验证数据面查询出错时本轮跳过：
// 把数据键换成 hash 类型注入 WRONGTYPE（bitmap ExistsMulti 的 Lua
// GETBIT 触发数据类错误）→ inner.ExistsMulti 返回 err → 不触发重建
// （区分"错误"与"失效"：错误结果不可信，不得当作丢数据证据）。
// 权威状态键类型不受影响 → loop 照常刷新本地新鲜 Ready，确保走到
// ExistsMulti 而非在相位检查就被 stale 拦下。
func TestBloomProbeExistsMultiErrorSkips(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("probe-wrongtype")
	const probeI = 80 * time.Millisecond

	var fnCalls, probeCalls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		fnCalls.Add(1)
		_, err := ingest.AddMulti(ctx, "probe-seed")
		return err
	}
	liveness := func(ctx context.Context) ([]any, error) {
		probeCalls.Add(1)
		return []any{"probe-seed"}, nil
	}
	pf, _, mr := newPrefillProbeForTest(t, key, pvReady, fn,
		WithSyncInterval(50*time.Millisecond),
		WithRebuildTimeout(2*time.Second),
		WithLivenessProbe(probeI, liveness))
	co := pf.coord
	if _, err := pf.inner.Add(ctx, "probe-seed"); err != nil {
		t.Fatalf("Add seed: %v", err)
	}
	co.storeLocal(PrefillReady)
	baseTries := co.acquireTries.Load()

	// 注入类型错误：数据键 DEL 后 HSet 成 hash → ExistsMulti GETBIT WRONGTYPE
	mr.Del(key)
	mr.HSet(key, "f", "v")

	time.Sleep(6 * probeI)
	if probeCalls.Load() < 1 {
		t.Fatalf("probeFn 应被调用（才走到 ExistsMulti），probeCalls=%d", probeCalls.Load())
	}
	// 报错时 ExistsMulti 必然出错（自检注入有效）：直查一次确认
	if _, err := pf.inner.ExistsMulti(ctx, "probe-seed"); err == nil {
		t.Fatal("前置条件：类型错误注入应使 ExistsMulti 返回 error")
	}
	if got := co.acquireTries.Load(); got != baseTries {
		t.Fatalf("ExistsMulti 错误不应触发重建（错误≠失效）：base=%d got=%d", baseTries, got)
	}
	if fnCalls.Load() != 0 {
		t.Fatalf("ExistsMulti 错误不应触发回灌 fn，fnCalls=%d", fnCalls.Load())
	}
	if v := mustGet(t, mr, co.stateKey); v != pvReady {
		t.Fatalf("状态应保持 ready，got %q", v)
	}
}

// --- 5) 探测触发的重建预算独立于探测 ctx（方案 A） ---

// TestBloomProbeRebuildBudgetIndependentOfInterval 验证失效探测触发的
// 重建以 RebuildTimeout 为预算，而非被探测 ctx 的 min(interval,30s) 钉死：
// probeInterval=50ms、回灌 fn 耗时 300ms（>50ms）时，重建必须成功收口
// ready 而非在 Reset 清空后超时留下 fail 现场（若 run 复用探测 ctx，
// fn 会在 50ms 被取消返回 DeadlineExceeded → finish 写 fail → 判红）。
func TestBloomProbeRebuildBudgetIndependentOfInterval(t *testing.T) {
	key := bloomTestKey("probe-budget")
	const probeI = 50 * time.Millisecond

	var fnCalls, probeCalls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		fnCalls.Add(1)
		// 回灌耗时 300ms > probeI=50ms：预算须为 RebuildTimeout(2s)
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := ingest.AddMulti(ctx, "probe-seed")
		return err
	}
	liveness := func(ctx context.Context) ([]any, error) {
		probeCalls.Add(1)
		return []any{"probe-seed"}, nil // 未回灌 → 必 miss → 失效
	}
	pf, _, mr := newPrefillProbeForTest(t, key, pvReady, fn,
		WithSyncInterval(20*time.Millisecond),
		WithRebuildTimeout(2*time.Second),
		WithLivenessProbe(probeI, liveness))
	co := pf.coord
	co.storeLocal(PrefillReady)
	// 故意不 Add "probe-seed"：Ready 态但样本查不到

	waitUntilCond(t, 5*time.Second, func() string {
		return "重建未收口 ready（run 预算疑被探测 ctx 钉死）：probeCalls=" +
			strconv.Itoa(int(probeCalls.Load())) +
			" fnCalls=" + strconv.Itoa(int(fnCalls.Load())) +
			" state=" + mustGet(t, mr, co.stateKey)
	}, func() bool {
		return fnCalls.Load() >= 1 && mustGet(t, mr, co.stateKey) == pvReady
	})
	waitLocalPhase(t, co, PrefillReady, 2*time.Second)
	if s := co.loadLocal(); s.phase != PrefillReady {
		t.Fatalf("回灌 300ms>probeI 的重建应收口本地 Ready，got %v", s.phase)
	}
}

// TestBloomProbeCloseCancelsInflightRun 验证方案 A 的 Close 取消语义：
// 探测触发的 run 以 WithoutCancel 派生（剥离探测 ctx 的 deadline/cancel）
// 后，仍须经 run 内 AfterFunc(c.baseCtx) 合并生命周期取消——Close 能取消
// 在飞 run（fn 阻塞等 ctx → Close → fn 返回 context.Canceled，状态因
// Canceled 禁写停留 building；若合并断裂，fn 只能等满 RebuildTimeout
// 返回 DeadlineExceeded → 判红）。
func TestBloomProbeCloseCancelsInflightRun(t *testing.T) {
	key := bloomTestKey("probe-close")
	const probeI = 50 * time.Millisecond

	fnStarted := make(chan struct{}, 1)
	fnDone := make(chan error, 1)
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		select { // 防重复触发时多次通知
		case fnStarted <- struct{}{}:
		default:
		}
		<-ctx.Done() // 阻塞至 Close 取消（RebuildTimeout=5s 兜底）
		fnDone <- ctx.Err()
		return ctx.Err()
	}
	liveness := func(ctx context.Context) ([]any, error) {
		return []any{"probe-seed"}, nil // 未回灌 → 必 miss → 失效触发
	}
	pf, _, mr := newPrefillProbeForTest(t, key, pvReady, fn,
		WithSyncInterval(20*time.Millisecond),
		WithRebuildTimeout(5*time.Second),
		WithLivenessProbe(probeI, liveness))
	co := pf.coord
	co.storeLocal(PrefillReady)

	select { // 探测失效 → run → Δ=40ms → fn 启动
	case <-fnStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("失效探测未在限期内触发重建并启动 fn")
	}
	if err := pf.Close(); err != nil { // 停 probeLoop + cancel baseCtx → 合并取消在飞 run
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-fnDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Close 应取消在飞 run 使 fn 返回 context.Canceled，got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close 后 fn 未被取消返回（WithoutCancel 派生的 run 未合并生命周期取消）")
	}
	// Canceled 禁写：状态停留 building，未被写成 ready/fail。
	if v := mustGet(t, mr, co.stateKey); v != pvBuilding {
		t.Fatalf("取消后状态应停留 building（禁写），got %q", v)
	}
}

// --- 6) inner.ExistsMulti panic：recover 收口，本轮跳过不触发重建 ---

// panicExistsMultiInner 包装真实 inner，ExistsMulti 恒 panic——模拟应用
// 坏样本经 writer 进入 c.inner.ExistsMulti 后 panic 的场景：该调用不在
// callProbeFn 的 recover 范围内，探测 goroutine 必须自行收口，否则崩进程。
type panicExistsMultiInner struct {
	prefillInner
	calls atomic.Int32
}

func (p *panicExistsMultiInner) ExistsMulti(ctx context.Context, items ...any) ([]bool, error) {
	p.calls.Add(1)
	panic("existsmulti exploded")
}

// TestBloomProbeExistsMultiPanicSkips 验证 ExistsMulti panic 由探测侧
// recover：probeLoop 存活、本轮跳过、不触发重建、测试进程不崩（若未
// recover，probeLoop goroutine panic 会崩整个测试进程 → 判红）。
func TestBloomProbeExistsMultiPanicSkips(t *testing.T) {
	const probeI = 60 * time.Millisecond
	const window = 5 * probeI

	var probeCalls, fnCalls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		fnCalls.Add(1)
		return nil
	}
	liveness := func(ctx context.Context) ([]any, error) {
		probeCalls.Add(1)
		return []any{"probe-seed"}, nil
	}
	// 桩经 wrap 在 coordinator 构造时注入（替换协调器直查面；数据面
	// 不被调用）：inner 字段出生即定格，构造后无任何写——与后台
	// probeLoop 的 c.inner 读无竞态（工厂路径运行期替换字段会被
	// -race 判为 DATA RACE）。
	var bad *panicExistsMultiInner
	co, mr := newProbeCoordinatorForTest(t, bloomTestKey("probe-ex-panic"), pvReady, fn,
		func(real prefillInner) prefillInner {
			bad = &panicExistsMultiInner{prefillInner: real}
			return bad
		},
		WithSyncInterval(50*time.Millisecond),
		WithRebuildTimeout(2*time.Second),
		WithLivenessProbe(probeI, liveness))
	base := co.acquireTries.Load()

	time.Sleep(window) // 若 panic 未被 recover，goroutine panic 会崩整个测试进程
	if probeCalls.Load() < 1 {
		t.Fatalf("probeFn 应被调用（才走到 ExistsMulti），probeCalls=%d", probeCalls.Load())
	}
	if bad.calls.Load() < 1 {
		t.Fatalf("ExistsMulti 桩应被调用（前置条件），calls=%d", bad.calls.Load())
	}
	if got := co.acquireTries.Load(); got != base {
		t.Fatalf("ExistsMulti panic 不应触发重建：base=%d got=%d", base, got)
	}
	if fnCalls.Load() != 0 {
		t.Fatalf("ExistsMulti panic 不应触发回灌 fn，fnCalls=%d", fnCalls.Load())
	}
	if v := mustGet(t, mr, co.stateKey); v != pvReady {
		t.Fatalf("状态应保持 ready，got %q", v)
	}
}

// --- 选项：非法值静默忽略、合法值采纳 ---

// TestBloomProbeOptionBounds 验证 WithLivenessProbe 对齐既有非法值惯例：
// interval<=0 或 fn==nil 静默忽略（保留零值 = 不启用探测）；合法值写入
// prefillConfig 平铺字段（probeInterval/probeFn）。
func TestBloomProbeOptionBounds(t *testing.T) {
	cfg := defaultPrefillConfig()
	if cfg.probeInterval != 0 || cfg.probeFn != nil {
		t.Fatalf("默认不应启用探测：interval=%v fn=%v", cfg.probeInterval, cfg.probeFn)
	}
	sample := func(ctx context.Context) ([]any, error) { return nil, nil }

	// 非法值静默忽略
	WithLivenessProbe(0, sample)(&cfg)
	WithLivenessProbe(-time.Second, sample)(&cfg)
	WithLivenessProbe(time.Second, nil)(&cfg)
	if cfg.probeInterval != 0 || cfg.probeFn != nil {
		t.Fatalf("非法值应静默忽略：interval=%v fn=%v", cfg.probeInterval, cfg.probeFn)
	}

	// 合法值采纳
	WithLivenessProbe(3*time.Second, sample)(&cfg)
	if cfg.probeInterval != 3*time.Second || cfg.probeFn == nil {
		t.Fatalf("合法值未采纳：interval=%v fnNil=%v", cfg.probeInterval, cfg.probeFn == nil)
	}
	// 再次非法调用不得清空已采纳的合法值（对齐既有 option 只写合法值惯例）
	WithLivenessProbe(0, nil)(&cfg)
	if cfg.probeInterval != 3*time.Second || cfg.probeFn == nil {
		t.Fatalf("非法值不应覆盖已采纳值：interval=%v fnNil=%v", cfg.probeInterval, cfg.probeFn == nil)
	}
}

// --- 7) cuckoo 侧对称：WithCuckooPrefill + WithLivenessProbe 接线回归 ---

// TestCuckooProbeFailureTriggersForceRebuild 锁定 cuckoo 形态的探测行为
// 回归：WithCuckooPrefill(fn, WithLivenessProbe(...)) 构造（miniredis 无
// CF.* 模块 → 走 hashImpl）→ 权威/本地 Ready 但肯定样本未回灌 → 探测
// 判失效 → 触发 force=1 重建 → 回灌 fn 被调 → 状态收口 ready、样本可
// 命中（PrefillOption 经 WithCuckooPrefill 的 opts 传入接线不得断裂）。
func TestCuckooProbeFailureTriggersForceRebuild(t *testing.T) {
	ctx := t.Context()
	key := cuckooTestKey("probe-fail")
	const probeI = time.Second

	var fnCalls, probeCalls atomic.Int32
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		fnCalls.Add(1)
		_, err := ingest.AddMulti(ctx, "probe-seed")
		return err
	}
	liveness := func(ctx context.Context) ([]any, error) {
		probeCalls.Add(1)
		return []any{"probe-seed"}, nil // 肯定样本但从未回灌 → inner 必 miss
	}
	rc, mr := newMiniRedisClient(t)
	stateKey, _ := prefillStateKeys(key)
	if err := rc.Set(ctx, stateKey, pvReady, 0).Err(); err != nil { // 先于构造：loop 首拍即见 ready
		t.Fatalf("seed state: %v", err)
	}
	f, err := rc.NewCuckooFilter(ctx, key, WithCuckooPrefill(fn,
		WithSyncInterval(20*time.Millisecond),
		WithRebuildTimeout(2*time.Second),
		WithLivenessProbe(probeI, liveness)))
	if err != nil {
		t.Fatalf("NewCuckooFilter: %v", err)
	}
	if f.coord == nil {
		t.Fatal("启用 WithCuckooPrefill 后应持有 coord")
	}
	co := f.coord
	co.storeLocal(PrefillReady) // 不 stopLoop：probeLoop 是被测对象
	t.Cleanup(func() {
		waitInflightDone(co, 3*time.Second)
		_ = f.Close()
	})
	// 故意不 Add "probe-seed"：Ready 态但样本查不到 → 探测失效

	waitUntilCond(t, 5*time.Second, func() string {
		return "cuckoo 失效探测未触发重建：probeCalls=" + strconv.Itoa(int(probeCalls.Load())) +
			" fnCalls=" + strconv.Itoa(int(fnCalls.Load())) +
			" state=" + mustGet(t, mr, co.stateKey)
	}, func() bool {
		return fnCalls.Load() >= 1 && mustGet(t, mr, co.stateKey) == pvReady
	})
	if probeCalls.Load() < 1 {
		t.Fatalf("probeFn 应至少被调一次，probeCalls=%d", probeCalls.Load())
	}
	waitLocalPhase(t, co, PrefillReady, 2*time.Second)
	if ok, err := f.Exists(ctx, "probe-seed"); err != nil || !ok {
		t.Fatalf("重建后样本应命中（回灌收口）: ok=%v err=%v", ok, err)
	}
}
