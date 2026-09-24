package redis

// G1 完整性校验的 cluster 形态补测（发布前双拓扑验证矩阵·阶段 3）。
// IntegrityProbe 的 bf/bitmap 判据走 pipeline（分片逐键 TYPE/STRLEN），
// cluster 下 go-redis cluster-pipeline 按节点分组执行——评审台账「需
// 实测项」。本文件验证性质：断言只读 + 单实例重建闭环，不改生产代码。
//
// 环境守卫与纪律对齐包内先例（cluster_integration_internal_test.go）：
// REDIS_CLUSTER 未设置或集群无 bf 模块即 Skip；键唯一前缀（randHex），
// 收尾逐键 Del（多键跨 slot 会 CROSSSLOT），严禁 FLUSHDB。分片 n=8
// 保证 pipeline 跨节点分组真实发生（6 节点集群）。

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// clusterIntegrityFixture 白盒直构 cluster 上的 BF 分片 + prefill 装饰器
// （路径强制 bfCmdImpl，与工厂 Capability 分派无关——bf 前提由守卫保证）。
type clusterIntegrityFixture struct {
	rc   *redisClient
	pf   *prefillFilter
	co   *prefillCoordinator
	base string
}

// requireClusterIntegrity BF 集群守卫：URL/探测/Skip 归属编排环境，
// 与 TestBloomABPathsShardedCluster 同法。fn 回灌 seeds 后返回装配。
func requireClusterIntegrity(t *testing.T, seed []any) *clusterIntegrityFixture {
	t.Helper()
	raw := os.Getenv("REDIS_CLUSTER")
	if raw == "" {
		t.Skip("REDIS_CLUSTER 未设置：跳过 G1 cluster 补测（集群 pipeline 形态无法承载）")
	}
	rdb, err := NewWithURL(raw)
	if err != nil {
		t.Fatalf("NewWithURL: %v", err)
	}
	_ = rdb.Capability().Probe(t.Context()) // 纯内存读：守卫前显式探测
	if !rdb.Capability().HasModule("bf") {
		_ = rdb.GracefulClose(context.Background())
		t.Skip("集群未加载 bf 模块：跳过 G1 cluster 补测（BF 路径前提不成立）")
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}
	if rc.Mode() != ModeCluster {
		t.Fatalf("连接模式应为 ModeCluster，got %v", rc.Mode())
	}

	ctx := context.Background()
	base := fmt.Sprintf("integrityct:%s", randHex(8))
	stateKey, failnKey := prefillStateKeys(base)

	cfg := defaultBloomConfig()
	WithCapacity(100_000)(&cfg)
	WithFalsePositive(0.01)(&cfg)
	WithShardCount(8)(&cfg)
	cfg.policy = FailOpen
	WithPrefill(func(ctx context.Context, ingest PrefillIngest) error {
		_, err := ingest.AddMulti(ctx, seed...)
		return err
	}, WithSyncInterval(50*time.Millisecond), WithRebuildTimeout(30*time.Second))(&cfg)

	bf := rc.newBFImpl(base, cfg) // 直构不含建键（工厂路径才 connectAll）
	if err := bf.connectAll(ctx); err != nil {
		t.Fatalf("bf connectAll（8 分片 BF.RESERVE 预建）: %v", err)
	}
	if !bf.sharder.enabled || bf.sharder.n != 8 {
		t.Fatalf("BF 分片未激活：enabled=%v n=%d", bf.sharder.enabled, bf.sharder.n)
	}
	pf := newPrefillFilter(rc, bf, base, cfg)
	fx := &clusterIntegrityFixture{rc: rc, pf: pf, co: pf.coord, base: base}
	fx.co.stopLoop() // 手动驱动时序（syncOnce 单写者确定）

	cleanupKeys := []string{base, stateKey, failnKey}
	cleanupKeys = append(cleanupKeys, bf.sharder.allKeys()...)
	t.Cleanup(func() {
		waitInflightDone(fx.co, 5*time.Second)
		_ = pf.Close()
		for _, k := range cleanupKeys { // 逐键 Del，严禁多键 DEL/FLUSHDB
			if err := rc.Del(ctx, k).Err(); err != nil {
				t.Logf("cleanup Del %s: %v", k, err)
			}
		}
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rc.GracefulClose(cctx)
	})
	return fx
}

// TestIntegrityClusterBFEBuild 用例 A：真机 cluster + BF 路径 + 分片 n=8。
// ready 打底 → **只 DEL 一个分片数据键**（状态键保留）→ syncOnce 经
// cluster 分组 pipeline 的 TYPE 检出 none → 本地即刻降级 → 异步 force=1
// 重建（Reset 全量重建 8 分片 + 回灌）→ 恢复新鲜 Ready → 真实 Exists
// 恢复、ghost false → 期间 acquireTries 恰 +1。
func TestIntegrityClusterBFEBuild(t *testing.T) {
	fx := requireClusterIntegrity(t, []any{"ctseed-1", "ctseed-2", "ctseed-3"})
	ctx := context.Background()
	co, rc := fx.co, fx.rc
	stateKey, _ := prefillStateKeys(fx.base)

	// 打底：inner 直写种子 + 权威 ready → syncOnce → 新鲜 Ready、
	// 8 分片 pipeline TYPE 全 MBbloom-- → probe ok、零重建。
	if _, err := fx.pf.inner.AddMulti(ctx, "ctseed-1", "ctseed-2", "ctseed-3"); err != nil {
		t.Fatalf("打底 AddMulti: %v", err)
	}
	if err := rc.Set(ctx, stateKey, pvReady, 0).Err(); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	co.syncOnce()
	if !co.readyFresh() {
		t.Fatalf("打底应收敛新鲜 Ready：%+v lastErr=%v", co.loadLocal(), co.phaseInfo().LastSyncErr)
	}
	baseTries := co.acquireTries.Load()
	if baseTries != 0 {
		t.Fatalf("打底健康态不应有任何抢占，acquireTries=%d", baseTries)
	}

	// 检出：只 DEL 第 3 分片数据键（其余 7 片完好——验证 pipeline 跨节点
	// 分组下对单片缺失的判别与不误报）。
	victim := fx.pf.inner.(*bfCmdImpl).sharder.allKeys()[3]
	if err := rc.Del(ctx, victim).Err(); err != nil {
		t.Fatalf("Del 分片键 %s: %v", victim, err)
	}
	co.syncOnce()
	if s := co.loadLocal(); s.phase == PrefillReady {
		t.Fatalf("单分片缺失经 cluster pipeline 应检出降级，相位仍 %v（lastErr=%v）", s.phase, co.phaseInfo().LastSyncErr)
	}

	// 异步 force=1 收敛：权威回 ready、本地新鲜 Ready、恰 +1 次抢占。
	waitAuthVal(t, rc, stateKey, pvReady, prefillITTimeout)
	waitLocalPhase(t, co, PrefillReady, prefillITTimeout)
	convergeFreshReady(t, co, rc)
	if got := co.acquireTries.Load(); got != baseTries+1 {
		t.Fatalf("单实例检出应恰一次 force=1 抢占，acquireTries %d→%d", baseTries, got)
	}

	// 真实查询恢复：种子命中、ghost false（8 分片重建+回灌完整）。
	for _, s := range []string{"ctseed-1", "ctseed-2", "ctseed-3"} {
		if ok, err := fx.pf.Exists(ctx, s); err != nil || !ok {
			t.Fatalf("恢复后种子 %s 应命中: ok=%v err=%v", s, ok, err)
		}
	}
	if ok, err := fx.pf.Exists(ctx, "ct-ghost-"+randHex(8)); err != nil || ok {
		t.Fatalf("恢复后 ghost 应真实 false: ok=%v err=%v", ok, err)
	}
	if info := co.phaseInfo(); info.LastSyncErr != nil {
		t.Fatalf("收敛后 LastSyncErr 应清零，got %v", info.LastSyncErr)
	}
}

// TestIntegrityClusterNoFalsePositive 用例 B：健康态（8 分片全在、无
// DEL）连跑 ≥5 拍 syncOnce——cluster 分组 pipeline 的命令聚合结果不得
// 被误判为结构证据（某节点上聚合错误若被折叠成 invalid，将表现为
// acquireTries 增量或降级，二者皆判别点）。
func TestIntegrityClusterNoFalsePositive(t *testing.T) {
	fx := requireClusterIntegrity(t, []any{"fpscore-1"})
	ctx := context.Background()
	co, rc := fx.co, fx.rc
	stateKey, _ := prefillStateKeys(fx.base)

	if _, err := fx.pf.inner.AddMulti(ctx, "fpscore-1"); err != nil {
		t.Fatalf("打底 AddMulti: %v", err)
	}
	if err := rc.Set(ctx, stateKey, pvReady, 0).Err(); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	// ≥6 拍（≥5 拍要求留 1 拍余量）：每拍 GET + 8 键 pipeline TYPE。
	for i := 0; i < 6; i++ {
		co.syncOnce()
	}
	if got := co.acquireTries.Load(); got != 0 {
		t.Fatalf("健康态 cluster pipeline 误报（acquireTries=%d≠0）——分组聚合被误判 invalid", got)
	}
	s := co.loadLocal()
	if s.phase != PrefillReady || !co.readyFresh() {
		t.Fatalf("健康态相位应保持新鲜 Ready：%+v", s)
	}
	if info := co.phaseInfo(); info.LastSyncErr != nil {
		t.Fatalf("健康态各拍应全成功（LastSyncErr=nil），got %v", info.LastSyncErr)
	}
}
