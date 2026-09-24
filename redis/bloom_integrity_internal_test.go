package redis

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// G1 完整性校验（v0.11.0 Issue #2，方案 B：键存在性/布局校验，无哨兵）
// 测试矩阵：规格 E 节 9 项，编号映射见各用例注释「矩阵 n」。白盒包内
// 测试 + 构造时注入桩（newProbeCoordinatorForTest 的 wrap——inner 字段
// 出生即定格，防运行期替换共享字段竞态；此类用例构造后立即 stopLoop，
// 保持「syncOnce 单写者」契约的确定性）。真机 gate 用例复用
// prefill_concurrency_integration_test.go 的基建与 Redis 侧锚定纪律。

// probeStubInner 覆盖 IntegrityProbe：计数 + 定制返回（矩阵 4b/5 判别
// probe 是否被调用、调用几次；hook 在构造时定格、测试期不再改）。
type probeStubInner struct {
	prefillInner
	hook  func(ctx context.Context) (bool, error)
	calls atomic.Int32
}

func (p *probeStubInner) IntegrityProbe(ctx context.Context) (bool, error) {
	p.calls.Add(1)
	return p.hook(ctx)
}

// --- 矩阵 1（P0）：hashImpl 空键合法——误判即每秒重建风暴回归 ---

// TestIntegrityHashEmptyKeyNoStorm hashImpl（cuckoo miniredis 回退路径）
// 键从未写入（TYPE none）+ 空 fn 达成 ready：连续多拍 syncOnce 搭车校验
// 必须判 ok（none 合法，ISSUE-101），acquireTries 零增量——若 none 误判
// invalid，每拍起 force=1 即风暴（in-flight CAS 顶多收敛到 +1，仍红）。
func TestIntegrityHashEmptyKeyNoStorm(t *testing.T) {
	key := cuckooTestKey("integ-storm")
	fn := func(context.Context, PrefillIngest) error { return nil }
	cf, _, mr := newCuckooPrefillForTest(t, key, fn, WithSyncInterval(50*time.Millisecond))
	co := cf.coord

	if err := mr.Set(co.stateKey, pvReady); err != nil {
		t.Fatal(err)
	}
	co.syncOnce()
	if s := co.loadLocal(); s.phase != PrefillReady {
		t.Fatalf("打底相位应 Ready，got %v", s.phase)
	}
	// 数据键保持从未创建（hashImpl 惰性建键 + 空数据源 ready 契约稳态）。

	base := co.acquireTries.Load()
	for i := 0; i < 5; i++ {
		co.syncOnce()
	}
	if got := co.acquireTries.Load(); got != base {
		t.Fatalf("hashImpl 空键必须合法（none=ok），acquireTries 应零增量：base=%d got=%d", base, got)
	}
	if s := co.loadLocal(); s.phase != PrefillReady {
		t.Fatalf("相位应保持 Ready（无降级），got %v", s.phase)
	}
	if info, _ := cf.Phase(); info.LastSyncErr != nil {
		t.Fatalf("校验全成功不应记错误，got %v", info.LastSyncErr)
	}
}

// --- 矩阵 2：四 impl 三态判据（miniredis 离线） ---

// TestIntegrityBitmapProbe bitmapImpl STRLEN 判据：足额长度=ok；缺失/
// 异类型占用=invalid（err=nil 结构性证据，WRONGTYPE 严禁折叠成传输错）；
// 传输错误=err 无结论（子测试独立 miniredis——Close 后不可复用）。
func TestIntegrityBitmapProbe(t *testing.T) {
	t.Run("结构判据", func(t *testing.T) {
		ctx := t.Context()
		rc, mr := newMiniRedisClient(t)
		f, err := rc.NewBloomFilter(ctx, bloomTestKey("integ-bmp"),
			WithCapacity(10_000), WithFalsePositive(0.0001))
		if err != nil {
			t.Fatal(err)
		}
		b := f.(*bitmapImpl)
		dataKey := b.sharder.allKeys()[0]

		// 工厂 connectAll 已全额建键 → ok（connectKeyLen 同源判据基线）。
		if ok, err := b.IntegrityProbe(ctx); err != nil || !ok {
			t.Fatalf("足额布局长度应 ok，got (%v, %v)", ok, err)
		}
		// 键缺失（STRLEN 0 ≠ wantLen）→ invalid、无错误。
		if !mr.Del(dataKey) {
			t.Fatal("前置：数据键应存在")
		}
		if ok, err := b.IntegrityProbe(ctx); err != nil || ok {
			t.Fatalf("键缺失应 (false, nil)，got (%v, %v)", ok, err)
		}
		// 异类型占用：STRLEN WRONGTYPE = 结构证据 invalid（err 必须 nil）。
		mr.HSet(dataKey, "f", "v")
		if ok, err := b.IntegrityProbe(ctx); err != nil || ok {
			t.Fatalf("WRONGTYPE 占用应 (false, nil) 结构证据，got (%v, %v)", ok, err)
		}
	})

	t.Run("传输错误无结论", func(t *testing.T) {
		ctx := t.Context()
		rc, mr := newMiniRedisClient(t)
		f, err := rc.NewBloomFilter(ctx, bloomTestKey("integ-bmp-ua"),
			WithCapacity(10_000), WithFalsePositive(0.0001))
		if err != nil {
			t.Fatal(err)
		}
		b := f.(*bitmapImpl)
		mr.Close() // 宕机：probe 的 STRLEN 走连接类错误
		if ok, err := b.IntegrityProbe(ctx); err == nil || ok || !IsUnavailable(err) {
			t.Fatalf("传输错误应 (false, Unavailable err)，got (%v, %v)", ok, err)
		}
	})
}

// TestIntegrityBfCfHashTypeProbe bf/cf TYPE 负面清单 + hashImpl
// {none, hash} 合法。bf/cf 的模块类型肯定态离线无法承载（miniredis 无
// BF/CF 模块），由负面清单纯函数断言 + 真机用例（TestIntegrityE2E…）互补。
func TestIntegrityBfCfHashTypeProbe(t *testing.T) {
	ctx := t.Context()
	rc, mr := newMiniRedisClient(t)
	bfKey := bloomTestKey("integ-bf")
	cfKey := cuckooTestKey("integ-cf")
	hsKey := cuckooTestKey("integ-hs")

	bf := &bfCmdImpl{client: rc, sharder: newBloomSharder(bfKey, false, 1)}
	cf := &cfCmdImpl{client: rc, key: cfKey}
	hs := &hashImpl{client: rc, key: hsKey}

	// 负面清单纯函数：Redis 核心值类型 invalid；模块类型名（真机 Redis
	// 8.8.2 实测 BF=MBbloom--、CF=MBbloomCF）与任意未知名 ok——
	// 禁硬编码模块类型名（ISSUE-103）。
	for _, neg := range []string{"none", "string", "hash", "list", "set", "zset", "stream"} {
		if !integrityTypeInvalid(neg) {
			t.Fatalf("负面清单应含 %q", neg)
		}
	}
	for _, mod := range []string{"MBbloom--", "MBbloomCF", "MBbloomSomething"} {
		if integrityTypeInvalid(mod) {
			t.Fatalf("模块类型名 %q 不得判 invalid（禁硬编码，ISSUE-103）", mod)
		}
	}

	// bf/cf：键缺失（none）与核心类型占用 → invalid。none 对 bf/cf 是
	// 失效证据（构造即建键），与 hashImpl 口径相反。
	if ok, err := bf.IntegrityProbe(ctx); err != nil || ok {
		t.Fatalf("bf 键缺失应 (false, nil)，got (%v, %v)", ok, err)
	}
	if err := mr.Set(bfKey, "junk"); err != nil {
		t.Fatal(err)
	}
	if ok, err := bf.IntegrityProbe(ctx); err != nil || ok {
		t.Fatalf("bf string 占用应 (false, nil)，got (%v, %v)", ok, err)
	}
	if ok, err := cf.IntegrityProbe(ctx); err != nil || ok {
		t.Fatalf("cf 键缺失应 (false, nil)，got (%v, %v)", ok, err)
	}
	mr.HSet(cfKey, "f", "v")
	if ok, err := cf.IntegrityProbe(ctx); err != nil || ok {
		t.Fatalf("cf hash 占用应 (false, nil)，got (%v, %v)", ok, err)
	}

	// hashImpl：none 与 hash 双形态 ok（ISSUE-101）；string 占用 invalid。
	if ok, err := hs.IntegrityProbe(ctx); err != nil || !ok {
		t.Fatalf("hash 键缺失（none）应合法 (true, nil)，got (%v, %v)", ok, err)
	}
	if err := mr.Set(hsKey, "x"); err != nil {
		t.Fatal(err)
	}
	if ok, err := hs.IntegrityProbe(ctx); err != nil || ok {
		t.Fatalf("hash 被 string 占用应 (false, nil)，got (%v, %v)", ok, err)
	}
	mr.Del(hsKey)
	mr.HSet(hsKey, "fp:1", "1")
	if ok, err := hs.IntegrityProbe(ctx); err != nil || !ok {
		t.Fatalf("hash 形态应 (true, nil)，got (%v, %v)", ok, err)
	}
}

// TestIntegrityTypeProbeUnavailable 矩阵 2 传输态补齐（ISSUE-205）：
// bf/cf/hash 三 impl 的 TYPE 路径在宕机下必须返回 (false, err) 且
// err 经 IsUnavailable 分类（无结论，严禁折叠成 invalid）——比照
// bitmap 传输错误子测模式。各 impl 独立 miniredis（mr.Close 后不可
// 复用），integrityTypeProbe/hashImpl 的 err 透传分支为共同判别点。
func TestIntegrityTypeProbeUnavailable(t *testing.T) {
	t.Run("bf", func(t *testing.T) {
		ctx := t.Context()
		rc, mr := newMiniRedisClient(t)
		bf := &bfCmdImpl{client: rc, sharder: newBloomSharder(bloomTestKey("integ-ua-bf"), false, 1)}
		if _, err := bf.IntegrityProbe(ctx); err != nil {
			t.Fatalf("前置：宕机前应正常回答，got %v", err) // 首条 TYPE 顺带暖连接
		}
		mr.Close()
		if ok, err := bf.IntegrityProbe(ctx); err == nil || ok || !IsUnavailable(err) {
			t.Fatalf("bf 传输错误应 (false, Unavailable err)，got (%v, %v)", ok, err)
		}
	})

	t.Run("cf", func(t *testing.T) {
		ctx := t.Context()
		rc, mr := newMiniRedisClient(t)
		cf := &cfCmdImpl{client: rc, key: cuckooTestKey("integ-ua-cf")}
		if _, err := cf.IntegrityProbe(ctx); err != nil {
			t.Fatalf("前置：宕机前应正常回答，got %v", err)
		}
		mr.Close()
		if ok, err := cf.IntegrityProbe(ctx); err == nil || ok || !IsUnavailable(err) {
			t.Fatalf("cf 传输错误应 (false, Unavailable err)，got (%v, %v)", ok, err)
		}
	})

	t.Run("hash", func(t *testing.T) {
		ctx := t.Context()
		rc, mr := newMiniRedisClient(t)
		hs := &hashImpl{client: rc, key: cuckooTestKey("integ-ua-hs")}
		if _, err := hs.IntegrityProbe(ctx); err != nil {
			t.Fatalf("前置：宕机前应正常回答，got %v", err)
		}
		mr.Close()
		if ok, err := hs.IntegrityProbe(ctx); err == nil || ok || !IsUnavailable(err) {
			t.Fatalf("hash 传输错误应 (false, Unavailable err)，got (%v, %v)", ok, err)
		}
	})
}

// --- 矩阵 8：bitmap STRLEN 边界 wantLen±1（布局判别力，防只查存在退化） ---

func TestIntegrityBitmapLenBoundary(t *testing.T) {
	ctx := t.Context()
	rc, mr := newMiniRedisClient(t)
	key := bloomTestKey("integ-bnd")
	f, err := rc.NewBloomFilter(ctx, key, WithCapacity(10_000), WithFalsePositive(0.0001))
	if err != nil {
		t.Fatal(err)
	}
	b := f.(*bitmapImpl)
	wantLen := b.connectKeyLen()
	if wantLen < 2 {
		t.Fatalf("测试前提：wantLen 应 ≥2 才有 -1 边界，got %d", wantLen)
	}

	for _, c := range []struct {
		name string
		len  int
		ok   bool
	}{
		{"wantLen 等值", int(wantLen), true},
		{"wantLen-1 短布局", int(wantLen) - 1, false},
		{"wantLen+1 长布局", int(wantLen) + 1, false},
	} {
		if err := mr.Set(key, string(make([]byte, c.len))); err != nil {
			t.Fatal(err)
		}
		if ok, err := b.IntegrityProbe(ctx); err != nil || ok != c.ok {
			t.Fatalf("%s: got (%v, %v) want ok=%v", c.name, ok, err, c.ok)
		}
	}
}

// --- 矩阵 3：端到端检出→降级→异步 force=1 重建→恢复（miniredis bitmap） ---

// TestIntegrityE2EDetectRebuild ready 后 DEL 数据键 → syncOnce 检出：
// 本地即刻降级（Uninitialized）+ 后台抢占（acquireTries+1，acquire Lua
// 成功即转 building）→ 重建完成恢复 Ready、Exists 恢复真实查询。风险③
// 归因（ISSUE-206）：同步实现的**主判别**是降级断言（syncOnce 若被
// run 阻塞至收口，相位将是 Building/Ready 而非 Uninitialized 即红）；
// UpdatedAt 前进断言为**次要面向**——防重建在飞期间本实例快照观测断供。
func TestIntegrityE2EDetectRebuild(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("integ-e2e")
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		_, err := ingest.AddMulti(ctx, "e2e-seed")
		return err
	}
	pf, _, mr := newPrefillFilterForTest(t, key, fn,
		WithSyncInterval(50*time.Millisecond), WithRebuildTimeout(2*time.Second))
	co := pf.coord

	// 打底：权威 ready + 数据键足额（工厂 connectAll 已建）→ 校验 ok 无重建。
	if err := mr.Set(co.stateKey, pvReady); err != nil {
		t.Fatal(err)
	}
	co.syncOnce()
	if s := co.loadLocal(); s.phase != PrefillReady {
		t.Fatalf("打底相位应 Ready，got %v", s.phase)
	}
	if got := co.acquireTries.Load(); got != 0 {
		t.Fatalf("校验 ok 拍不得触发重建，acquireTries=%d", got)
	}

	// 检出：DEL 数据键 → 本拍 invalid。
	mr.Del(key)
	co.syncOnce()
	// C-2 本地即刻降级（反向验证点位：删除该行此断言必红）——Exists
	// 恢复恒 true，阻断假阴性透传。同时此断言也是**同步实现的判别点**
	// （ISSUE-206 归因：若 run(force=1) 被误实现为同步，本拍 syncOnce
	// 阻塞至重建收口，相位将是 Building/Ready 而非 Uninitialized，此断
	// 言红）。
	if s := co.loadLocal(); s.phase != PrefillUninitialized {
		t.Fatalf("invalid 检出后本地应即刻降级 Uninitialized，got %v", s.phase)
	}
	if ok, err := pf.Exists(ctx, "e2e-ghost"); err != nil || !ok {
		t.Fatalf("降级期 Exists 应恒 (true,nil)，got (%v, %v)", ok, err)
	}

	// UpdatedAt 前进断言（次要面向）：防重建在飞期间快照断供——异步
	// run 下后续拍 syncOnce 必须照常 GET/storeLocal（ISSUE-206 归因：
	// 同步实现的判别主责在上方降级断言，本断言专测在飞期观测连续性）。
	before := co.loadLocal().updatedAt
	co.syncOnce()
	if !co.loadLocal().updatedAt.After(before) {
		t.Fatal("重建在飞期间 syncOnce 仍须更新 UpdatedAt（快照观测断供即判别失败）")
	}

	// 后台收敛：权威 ready、本地恢复新鲜 Ready、恰一次抢占。
	waitPrefillState(t, mr, co.stateKey, pvReady, 5*time.Second)
	waitLocalPhase(t, co, PrefillReady, 5*time.Second)
	if got := co.acquireTries.Load(); got != 1 {
		t.Fatalf("恰好一次 force=1 重建，acquireTries=%d", got)
	}
	if !co.readyFresh() {
		t.Fatal("恢复后应新鲜 Ready")
	}
	// Exists 恢复真实查询：回灌项 true、ghost false。
	if ok, err := pf.Exists(ctx, "e2e-seed"); err != nil || !ok {
		t.Fatalf("回灌项应命中，got (%v, %v)", ok, err)
	}
	if ok, err := pf.Exists(ctx, "e2e-ghost"); err != nil || ok {
		t.Fatalf("恢复真实查询后 ghost 应 false，got (%v, %v)", ok, err)
	}
}

// TestIntegrityE2EBfModuleRealRedis 真机 BF.* 路径端到端（矩阵 3 真机版）：
// bfCmdImpl TYPE 判据的肯定态（MBbloom--）只有真机能承载——若负面清单
// 误伤模块类型名，冷启动重建后每拍检出 invalid 无限重触发，acquireTries
// 断言兜底判别。
func TestIntegrityE2EBfModuleRealRedis(t *testing.T) {
	url := requirePrefillRedis(t)
	ctx := context.Background()
	obs := dialPrefillClient(t, url)
	if err := obs.Capability().Probe(ctx); err != nil {
		t.Fatalf("Capability Probe: %v", err)
	}
	if !obs.Capability().HasBloom() {
		t.Skip("server has no bf module; skip bfCmdImpl integrity e2e")
	}

	rc := dialPrefillClient(t, url)
	if err := rc.Capability().Probe(ctx); err != nil {
		t.Fatalf("实例 Capability Probe: %v", err)
	}
	if !rc.Capability().HasBloom() {
		t.Skip("实例 Probe 后 HasBloom=false；skip bfCmdImpl integrity e2e")
	}
	key := bloomTestKey("integ-bfe2e")
	stateKey, failnKey := prefillStateKeys(key)
	prefillCleanupKeys(t, obs, key, stateKey, failnKey)
	if err := obs.Del(ctx, key, stateKey, failnKey).Err(); err != nil {
		t.Fatalf("预清理: %v", err)
	}

	fn := func(ctx context.Context, ingest PrefillIngest) error {
		_, err := ingest.AddMulti(ctx, "bfe2e-seed")
		return err
	}
	pf := newFactoryPrefillReal(t, rc, key, fn,
		WithSyncInterval(50*time.Millisecond), WithRebuildTimeout(10*time.Second))
	co := pf.coord
	if _, isBF := pf.inner.(*bfCmdImpl); !isBF {
		t.Fatalf("HasBloom 下应分派 bfCmdImpl，got %T", pf.inner)
	}
	co.stopLoop() // 手动驱动时序

	// 冷启动：热路径 force=0 重建 → 收敛 ready。
	if _, err := pf.Exists(ctx, "bfe2e-ghost"); err != nil {
		t.Fatalf("热路径触发 Exists: %v", err)
	}
	waitAuthVal(t, obs, stateKey, pvReady, prefillITTimeout)
	waitLocalPhase(t, co, PrefillReady, prefillITTimeout)
	coldTries := co.acquireTries.Load()
	if coldTries != 1 {
		t.Fatalf("冷启动应恰一次重建，acquireTries=%d（校验误判模块类型即反复重建）", coldTries)
	}
	if ok, err := pf.Exists(ctx, "bfe2e-ghost"); err != nil || ok {
		t.Fatalf("新鲜 Ready 下 ghost 应真实 false（probe 不得把 MBbloom-- 判 invalid），got (%v, %v)", ok, err)
	}

	// 检出：DEL 数据键 → syncOnce → 本地降级 + force=1 → 恢复。
	if err := obs.Del(ctx, key).Err(); err != nil {
		t.Fatalf("Del BF 数据键: %v", err)
	}
	co.syncOnce()
	if s := co.loadLocal(); s.phase != PrefillUninitialized {
		t.Fatalf("bf 检出后应即刻降级，got %v", s.phase)
	}
	waitAuthVal(t, obs, stateKey, pvReady, prefillITTimeout)
	waitLocalPhase(t, co, PrefillReady, prefillITTimeout)
	if got := co.acquireTries.Load(); got != coldTries+1 {
		t.Fatalf("检出后恰一次 force=1 重建，acquireTries %d→%d", coldTries, got)
	}
	if ok, err := pf.Exists(ctx, "bfe2e-seed"); err != nil || !ok {
		t.Fatalf("恢复后回灌项应命中，got (%v, %v)", ok, err)
	}
}

// --- 矩阵 4：Unavailable 无结论（GET 级 + probe 级） ---

func TestIntegrityUnavailableNoConclusion(t *testing.T) {
	t.Run("GET 级宕机", func(t *testing.T) {
		key := bloomTestKey("integ-unavail-get")
		fn := func(context.Context, PrefillIngest) error { return nil }
		pf, _, mr := newPrefillFilterForTest(t, key, fn, WithSyncInterval(50*time.Millisecond))
		co := pf.coord
		if err := mr.Set(co.stateKey, pvReady); err != nil {
			t.Fatal(err)
		}
		co.syncOnce() // 打底 + probe ok（数据键足额）
		before := co.loadLocal()
		if before.phase != PrefillReady {
			t.Fatalf("打底相位应 Ready，got %v", before.phase)
		}

		mr.Close()
		co.syncOnce() // GET 传输失败（probe 不达）
		info, err := pf.Phase()
		if err != nil {
			t.Fatal(err)
		}
		if info.LastSyncErr == nil || !IsUnavailable(info.LastSyncErr) {
			t.Fatalf("宕机拍应记 Unavailable 错误，got %v", info.LastSyncErr)
		}
		if s := co.loadLocal(); s.phase != PrefillReady || !s.updatedAt.Equal(before.updatedAt) {
			t.Fatalf("传输错误不得降级/改写快照，got %+v", s)
		}
		if co.acquireTries.Load() != 0 {
			t.Fatal("传输错误拍不得触发重建")
		}
	})

	t.Run("probe 级无结论", func(t *testing.T) {
		key := bloomTestKey("integ-unavail-probe")
		fn := func(context.Context, PrefillIngest) error { return nil }
		sentinel := errors.New("dial tcp 10.0.0.1:6379: connect: connection refused (probe stub)")
		stub := &probeStubInner{hook: func(context.Context) (bool, error) { return false, sentinel }}
		co, _ := newProbeCoordinatorForTest(t, key, pvReady, fn,
			func(real prefillInner) prefillInner {
				stub.prefillInner = real
				return stub
			},
			WithSyncInterval(50*time.Millisecond), WithRebuildTimeout(2*time.Second))
		co.stopLoop() // 手动一拍即够：保持 syncOnce 单写者确定时序

		base := co.acquireTries.Load()
		co.syncOnce() // GET ready → probe err
		if stub.calls.Load() != 1 {
			t.Fatalf("ready 拍应恰好一次 probe，calls=%d", stub.calls.Load())
		}
		info := co.phaseInfo()
		if info.LastSyncErr != sentinel { // == 直等断言：原样存储不包装
			t.Fatalf("probe 传输错误应原样记录（==同实例不包装），got %v", info.LastSyncErr)
		}
		if !errors.Is(info.LastSyncErr, sentinel) {
			t.Fatalf("LastSyncErr 应 errors.Is 可感知，got %v", info.LastSyncErr)
		}
		if info.LastSyncErrAt.IsZero() {
			t.Fatal("记录错误时 at 必须非零")
		}
		if s := co.loadLocal(); s.phase != PrefillReady {
			t.Fatalf("无结论不得降级，got %v", s.phase)
		}
		if got := co.acquireTries.Load(); got != base {
			t.Fatalf("无结论不得重建，acquireTries %d→%d", base, got)
		}
	})
}

// --- 矩阵 5：building/fail/uninitialized 相位不校验 ---

// TestIntegrityNonReadyNoProbe building 态 DEL 数据键（probe 若被调必
// invalid）→ syncOnce 无任何校验搭载：无 probe 调用、仅 1 条 GET、
// 相位/acquireTries 不动。
func TestIntegrityNonReadyNoProbe(t *testing.T) {
	key := bloomTestKey("integ-building")
	fn := func(context.Context, PrefillIngest) error { return nil }
	stub := &probeStubInner{hook: func(context.Context) (bool, error) { return false, nil }}
	co, mr := newProbeCoordinatorForTest(t, key, pvBuilding, fn,
		func(real prefillInner) prefillInner {
			stub.prefillInner = real
			return stub
		},
		WithSyncInterval(50*time.Millisecond), WithRebuildTimeout(2*time.Second))
	co.stopLoop() // 时序确定性（本实例 syncOnce 单写者）

	mr.Del(key) // building 期键本就在 DEL/重建，校验必误报——不得发 probe

	n0 := mr.CommandCount()
	base := co.acquireTries.Load()
	co.syncOnce()
	if got := mr.CommandCount() - n0; got != 1 {
		t.Fatalf("building 拍应仅 1 条 GET（校验未搭载），实测 %d 条", got)
	}
	if stub.calls.Load() != 0 {
		t.Fatalf("building 相位不得调 IntegrityProbe，calls=%d", stub.calls.Load())
	}
	if s := co.loadLocal(); s.phase != PrefillBuilding {
		t.Fatalf("building 相位应保持不动，got %v", s.phase)
	}
	if got := co.acquireTries.Load(); got != base {
		t.Fatalf("building 拍不得触发重建，acquireTries %d→%d", base, got)
	}
}

// convergeFreshReady 手动驱动 syncOnce 直至本地收敛"新鲜 Ready"（loop
// 已停的 gate 用例专用：败者/后到者相位刷新只能靠 syncOnce 本身——
// 每拍兼作 G1 校验，权威 ready + 数据键足额即恢复）。
func convergeFreshReady(t *testing.T, co *prefillCoordinator, obs *redisClient) {
	t.Helper()
	deadline := time.Now().Add(prefillITTimeout)
	for !co.readyFresh() {
		if time.Now().After(deadline) {
			t.Fatalf("手动驱动未收敛新鲜 Ready：%+v lastErr=%v", co.loadLocal(), co.phaseInfo().LastSyncErr)
		}
		co.syncOnce()
		time.Sleep(20 * time.Millisecond)
	}
}

// --- 矩阵 6：多实例仲裁（真机 gate）——双双检出 → 恰一个执行者 ---

// TestIntegrityDualInstanceArbitrationRealRedis 两实例冷启动齐射（计数=1）
// 后 DEL 数据键、两实例各一拍 syncOnce 双双检出 invalid → 各起异步
// force=1——acquire Lua 真 Redis 原子仲裁：building 窗内（fn 150ms 撑大）
// 后到者被拒 ErrRebuildInProgress 静默 → 本轮检出 fn 恰执行一次（计数
// 封顶 2）；败者本地降级态经后续 syncOnce 自愈恢复 Ready（ISSUE-107③）。
func TestIntegrityDualInstanceArbitrationRealRedis(t *testing.T) {
	url := requirePrefillRedis(t)
	ctx := context.Background()
	obs := dialPrefillClient(t, url)

	key := bloomTestKey("integ-dual")
	stateKey, failnKey := prefillStateKeys(key)
	countKey := key + ":__runs"
	prefillCleanupKeys(t, obs, key, stateKey, failnKey, countKey)
	if err := obs.Del(ctx, key, stateKey, failnKey, countKey).Err(); err != nil {
		t.Fatalf("预清理: %v", err)
	}

	newFn := func(rc *redisClient) PrefillFunc {
		return func(ctx context.Context, ingest PrefillIngest) error {
			pIncr(t, rc, countKey)
			time.Sleep(150 * time.Millisecond) // 撑大 building 窗，保证双齐射撞锁
			_, err := ingest.AddMulti(ctx, "dual-seed")
			return err
		}
	}

	insts := make([]*prefillFilter, 2)
	for i := range insts {
		rc := dialPrefillClient(t, url)
		pf := newFactoryPrefillReal(t, rc, key, newFn(rc),
			WithSyncInterval(50*time.Millisecond), WithRebuildTimeout(10*time.Second))
		pf.coord.stopLoop() // 检出轮由测试手动 syncOnce 齐射
		insts[i] = pf
	}

	// 冷启动：两实例同刻热路径齐射 → 恰一次 fn（计数=1）。
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, pf := range insts {
		wg.Add(1)
		go func(pf *prefillFilter) {
			defer wg.Done()
			<-start
			_, _ = pf.Exists(context.Background(), "dual-ghost")
		}(pf)
	}
	close(start)
	wg.Wait()
	waitAuthVal(t, obs, stateKey, pvReady, prefillITTimeout)
	waitRunsCount(t, obs, countKey, 1, 3*time.Second)
	// 败者本地收敛依赖 syncOnce（tick 已停，同模板用例 1 教训）：手动
	// 驱动至新鲜 Ready——收敛期内若撞 winner 重建中的 invalid，其
	// force=1 必被 building 拒绝（fn 不补跑，计数不受扰）。
	convergeFreshReady(t, insts[0].coord, obs)
	convergeFreshReady(t, insts[1].coord, obs)

	// 检出：DEL 数据键 → 两实例各一拍 syncOnce → 各起异步 force=1。
	// 后到者 acquire 必落 winner 的 building 窗（µs 级起跳 vs 150ms fn），
	// 被 Lua 原子拒绝——本轮 fn 仅 +1。
	if err := obs.Del(ctx, key).Err(); err != nil {
		t.Fatalf("Del 数据键: %v", err)
	}
	for _, pf := range insts {
		pf.coord.syncOnce()
	}
	waitRunsCount(t, obs, countKey, 2, prefillITTimeout)
	time.Sleep(400 * time.Millisecond) // 稳定窗：败者不得补跑
	if v := pGET(t, obs, countKey); v != "2" {
		t.Fatalf("多实例仲裁：本轮检出 fn 应恰执行一次，计数键=%s（期望 2）", v)
	}

	// 双实例收敛新鲜 Ready（败者经下拍 syncOnce 自愈）。
	for i, pf := range insts {
		deadline := time.Now().Add(prefillITTimeout)
		for !pf.coord.readyFresh() {
			if time.Now().After(deadline) {
				t.Fatalf("实例 %d 未收敛新鲜 Ready：%+v", i, pf.coord.loadLocal())
			}
			pf.coord.syncOnce()
			time.Sleep(20 * time.Millisecond)
		}
	}
	if ok, err := insts[0].Exists(ctx, "dual-seed"); err != nil || !ok {
		t.Fatalf("恢复后回灌项应命中，got (%v, %v)", ok, err)
	}
}

// --- 矩阵 7：装饰器透传——prefillFilter.IntegrityProbe 委托 inner ---

func TestIntegrityDecoratorPassthrough(t *testing.T) {
	ctx := t.Context()
	key := bloomTestKey("integ-passthru")
	fn := func(context.Context, PrefillIngest) error { return nil }
	pf, _, mr := newPrefillFilterForTest(t, key, fn, WithSyncInterval(50*time.Millisecond))

	// 工厂建键足额 → 装饰器入口透传真实判据 ok。
	if ok, err := pf.IntegrityProbe(ctx); err != nil || !ok {
		t.Fatalf("装饰器透传应 (true, nil)，got (%v, %v)", ok, err)
	}
	// DEL → inner 真实 invalid（证据透传，不经降级分派短路）。
	mr.Del(key)
	if ok, err := pf.IntegrityProbe(ctx); err != nil || ok {
		t.Fatalf("数据键缺失应 (false, nil)，got (%v, %v)", ok, err)
	}
}

// --- 矩阵 9：并发（-race）——搭车校验与异步 run/storeLocal 并发 ---

// TestIntegrityConcurrentRebuild syncOnce（内含 probe 检出→异步 force=1
// →finish storeLocal）与 Phase/readyFresh/热路径 Exists 并发驱动，
// -race 无告警；停止注入后收敛新鲜 Ready 且错误清零。
func TestIntegrityConcurrentRebuild(t *testing.T) {
	key := bloomTestKey("integ-race")
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		_, err := ingest.AddMulti(ctx, "race-seed")
		return err
	}
	pf, _, mr := newPrefillFilterForTest(t, key, fn,
		WithSyncInterval(10*time.Millisecond), WithRebuildTimeout(5*time.Second))
	co := pf.coord
	if err := mr.Set(co.stateKey, pvReady); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// A：本实例唯一 syncOnce 写者（单写者契约）；每 5 拍 DEL 数据键制造
	// invalid 检出→异步重建与后续拍并发的交错现场。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%5 == 0 {
				mr.Del(key)
			}
			co.syncOnce()
		}
	}()
	// B/C/D：读面与热路径并发。
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = pf.Phase()
				_ = co.readyFresh()
				_, _ = pf.Exists(context.Background(), "race-seed")
			}
		}()
	}
	time.Sleep(600 * time.Millisecond)
	close(stop)
	wg.Wait()
	waitInflightDone(co, 5*time.Second)

	// 收敛：注入已停，数据键将被下一次成功重建补回足额；轮询手动拍直至
	// 新鲜 Ready 且 LastSyncErr 清零。
	deadline := time.Now().Add(10 * time.Second)
	for !co.readyFresh() {
		if time.Now().After(deadline) {
			t.Fatalf("并发后未收敛新鲜 Ready：%+v err=%v", co.loadLocal(), co.phaseInfo().LastSyncErr)
		}
		co.syncOnce()
		waitInflightDone(co, 2*time.Second)
	}
	if info := co.phaseInfo(); info.LastSyncErr != nil {
		t.Fatalf("收敛后 LastSyncErr 应清零，got %v（at=%v）", info.LastSyncErr, info.LastSyncErrAt)
	}
}
