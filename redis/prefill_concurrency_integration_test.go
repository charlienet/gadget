package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Bloom prefill 端到端并发集成测试（真 Redis gate，对齐
// TestBloomPrefillDualInstanceMutexRealRedis / TestCuckooPrefillCfCmdResetRealRedis
// 的既有纪律：REDIS_URL 未设置即 Skip；随机前缀键 + 收尾逐键 Del，严禁
// FLUSHDB；多实例 = 同进程多个 redisClient 连同一 URL）。
//
// 覆盖四类上轮 race 修复的并发语义：
//  1. 多实例自动触发并发抢占（ticker/惰性齐射 → 全集群 fn 恰好一次；
//     含退避齐射：fail 记账全局一致 → 到期齐射仅一个新执行者）；
//  2. 重建期混合读写流量的降级正确性（Exists 无假阴、Add 放行、
//     Building 期不发抢锁风暴、完成后 ≤2 tick 收敛）；
//  3. 自动预填 → Reset → 数据丢失 + 有效性探测自愈 的完整生命周期串联
//     （探测触发的重建预算独立于 probeInterval，经真实慢 fn 验证）；
//  4. bfCmdImpl（BF.* 模块）路径的 prefill 抢占与探测（无模块环境 Skip）。
//
// 多实例执行计数一律锚定 Redis 侧事实：fn 内 INCR 服务端计数键
// （非单进程内存计数——同进程多 client 也须以 Redis 值为准）；
// failn/fail TTL 直接 GET/PTTL 权威键观测。断言全部 eventually
// （限期轮询 + 超时 Fatal 打现场快照），不 sleep 猜时序。

// --- 测试基建 ---

// prefillITTimeout 是集成用例内单项 eventually 断言的宽松预算，
// 防真 Redis 偶发抖动导致 flaky。
const prefillITTimeout = 15 * time.Second

// requirePrefillRedis REDIS_URL 守卫：未设置即 Skip（包内真 Redis
// integration 既有纪律）。
func requirePrefillRedis(t *testing.T) string {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skip real-Redis prefill concurrency test")
	}
	return url
}

// dialPrefillClient 构建独立 redisClient 实例（多实例 = 每实例一个
// client）并以 Ping 重试兜底环境偶发 dial 抖动（重试属测试基建层，
// 5s 预算内不成功才 Fatal）；t.Cleanup GracefulClose（级联停 coordinator）。
func dialPrefillClient(t *testing.T, url string) *redisClient {
	t.Helper()
	c, err := NewWithURL(url)
	if err != nil {
		t.Fatalf("NewWithURL: %v", err)
	}
	rc, ok := c.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", c)
	}
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err = rc.Ping(ctx).Err(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Ping 真 Redis 失败（基建层 5s 重试耗尽）: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() { _ = rc.GracefulClose(context.Background()) })
	return rc
}

// prefillCleanupKeys 注册收尾逐键 Del（含 :__prefill 两键族与计数键）。
// 单机 URL 无 CROSSSLOT 问题，仍逐键 Del 对齐包内纪律。
func prefillCleanupKeys(t *testing.T, c *redisClient, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, k := range keys {
			if err := c.Del(ctx, k).Err(); err != nil {
				t.Logf("cleanup Del %s: %v（残留容忍，键名随机不撞他人）", k, err)
			}
		}
	})
}

// prefillEventually 限期轮询 cond 直至真；超时 Fatal 并打印末次现场快照
// （eventually 断言的统一底座，10ms 轮询间隔）。
func prefillEventually(t *testing.T, desc string, timeout time.Duration, cond func() (ok bool, snapshot string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := "never-evaluated"
	for time.Now().Before(deadline) {
		var ok bool
		ok, last = cond()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("eventually [%s] 超时（%v）：末次现场 %s", desc, timeout, last)
}

// pGET 读字符串键；缺失归一为空串（ready/building/fail 均非空，无歧义）。
func pGET(t *testing.T, c *redisClient, key string) string {
	t.Helper()
	v, err := c.Get(context.Background(), key).Result()
	if errors.Is(err, goredis.Nil) {
		return ""
	}
	if err != nil {
		t.Fatalf("GET %s: %v", key, err)
	}
	return v
}

// pIncr 服务端原子计数（fn 内锚定"执行过"事实专用）。计数语义是
// "fn 被进入"，用脱离 run ctx 的 Background——run 被取消不应令计数丢失
// 或虚增口径漂移。
func pIncr(t *testing.T, c *redisClient, key string) {
	t.Helper()
	if err := c.Incr(context.Background(), key).Err(); err != nil {
		t.Fatalf("INCR %s: %v", key, err)
	}
}

// waitAuthVal 轮询权威状态键直至期望值；want=="" 表示等待键缺失。
func waitAuthVal(t *testing.T, c *redisClient, stateKey, want string, timeout time.Duration) {
	t.Helper()
	prefillEventually(t, fmt.Sprintf("权威状态键 %s=%q", stateKey, want), timeout, func() (bool, string) {
		v := pGET(t, c, stateKey)
		return v == want, fmt.Sprintf("stateKey=%s 当前值=%q", stateKey, v)
	})
}

// waitRunsCount 轮询 Redis 侧执行计数键直至期望值。
func waitRunsCount(t *testing.T, c *redisClient, countKey string, want int64, timeout time.Duration) {
	t.Helper()
	prefillEventually(t, fmt.Sprintf("Redis 侧执行计数 %s=%d", countKey, want), timeout, func() (bool, string) {
		v := pGET(t, c, countKey)
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			n = 0
		}
		return n == want, fmt.Sprintf("countKey=%s 当前=%q", countKey, v)
	})
}

// waitReadyFresh 限期等待协调器本地收敛"新鲜 Ready"（真实查询面的前提；
// 复用既有 waitLocalPhase 锚定相位，再等 readyFresh 的 age 条件）。
func waitReadyFresh(t *testing.T, co *prefillCoordinator, timeout time.Duration) {
	t.Helper()
	waitLocalPhase(t, co, PrefillReady, timeout)
	prefillEventually(t, "本地收敛新鲜 Ready", timeout, func() (bool, string) {
		s := co.loadLocal()
		return co.readyFresh(), fmt.Sprintf("phase=%v age=%v stale>=%v", s.phase, time.Since(s.updatedAt), 2*co.cfg.syncInterval)
	})
}

// newBitmapPrefillReal 白盒装配 bitmap 路径 + prefill 装饰器（与工厂
// cfg.enabled && fn!=nil 分支同构）。不走 NewBloomFilter 工厂：本环境
// 加载 bf 模块时工厂 auto 恒选 BF.* 路径，bitmap 路径的真 Redis 并发
// 覆盖须显式装配（用例 4 专锁 BF.* 路径，两路径互补）。
func newBitmapPrefillReal(t *testing.T, rc *redisClient, key string, fn PrefillFunc, opts ...PrefillOption) *prefillFilter {
	t.Helper()
	cfg := defaultBloomConfig()
	cfg.capacity = 10_000
	cfg.falsePositive = 0.0001
	cfg.enabled = true
	cfg.fn = fn
	for _, o := range opts {
		o(&cfg.prefillConfig)
	}
	inner := newBitmapImpl(rc, key, cfg)
	if err := inner.connectAll(context.Background()); err != nil {
		t.Fatalf("bitmapImpl connectAll: %v", err)
	}
	pf := newPrefillFilter(rc, inner, key, cfg)
	t.Cleanup(func() {
		// 先等在飞重建退出再 Close（对齐 newPrefillFilterForTest 纪律）
		waitInflightDone(pf.coord, 3*time.Second)
		_ = pf.Close()
	})
	return pf
}

// newFactoryPrefillReal 经工厂构造 prefill 过滤器（能力分派：bf 模块
// 在场即 bfCmdImpl——用例 4 用），并断言装饰器类型。
func newFactoryPrefillReal(t *testing.T, rc *redisClient, key string, fn PrefillFunc, opts ...PrefillOption) *prefillFilter {
	t.Helper()
	f, err := rc.NewBloomFilter(context.Background(), key,
		WithCapacity(10_000), WithFalsePositive(0.0001),
		WithPrefill(fn, opts...))
	if err != nil {
		t.Fatalf("NewBloomFilter: %v", err)
	}
	pf, ok := f.(*prefillFilter)
	if !ok {
		t.Fatalf("应为 *prefillFilter，got %T", f)
	}
	t.Cleanup(func() {
		waitInflightDone(pf.coord, 3*time.Second)
		_ = pf.Close()
	})
	return pf
}

// authValueSampler 后台轮询采样字符串键值（含 TTL 记录），用于捕获
// building/fail 等瞬态相位——"观测某值出现过"的事件型断言底座。
// stopCh 由使用方关闭（幂等）→ goroutine 退出时 close(doneCh)。
type authValueSampler struct {
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
	mu       sync.Mutex
	seen     map[string]int
	failMax  time.Duration // 值为 "fail" 时观测到的最大 PTTL
}

func startAuthValueSampler(t *testing.T, c *redisClient, key string) *authValueSampler {
	t.Helper()
	s := &authValueSampler{
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
		seen:   map[string]int{},
	}
	go func() {
		defer close(s.doneCh)
		ctx := context.Background()
		for {
			select {
			case <-s.stopCh:
				return
			default:
			}
			v, err := c.Get(ctx, key).Result()
			if errors.Is(err, goredis.Nil) {
				v = ""
			}
			if err == nil || errors.Is(err, goredis.Nil) {
				s.mu.Lock()
				s.seen[v]++
				if v == pvFail {
					if pttl := c.PTTL(ctx, key).Val(); pttl > s.failMax {
						s.failMax = pttl
					}
				}
				s.mu.Unlock()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	t.Cleanup(s.stop)
	return s
}

func (s *authValueSampler) stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	<-s.doneCh // 等 goroutine 真正退出，防测试结束后残留命令
}

func (s *authValueSampler) snapshot() (seen map[string]int, failMax time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(map[string]int, len(s.seen))
	for k, v := range s.seen {
		cp[k] = v
	}
	return cp, s.failMax
}

// --- 用例 1：多实例自动触发并发抢占 + 退避齐射 ---

// TestPrefillConcurrencyMultiInstanceClaim 真 Redis 上 4 实例（4 个
// client、同键、短 syncInterval）冷启动并发抢锁：回灌 fn 全集群恰好
// 执行 1 次（Redis 侧 INCR 计数锚定）、败者静默不排队、全部本地收敛
// Ready、权威=ready、各实例真实查询命中回灌数据。子场景"退避齐射"：
// 首次 fn 失败 → failn/fail TTL 全局一致 → 退避到期齐射仅一个新执行者
// （Redis 侧执行计数恰好 2 封顶）。
func TestPrefillConcurrencyMultiInstanceClaim(t *testing.T) {
	url := requirePrefillRedis(t)

	const (
		nInst  = 4
		syncI  = 50 * time.Millisecond
		seedsN = 3
	)
	seeds := make([]any, seedsN)
	for i := range seeds {
		seeds[i] = fmt.Sprintf("pf-conc-seed-%d", i)
	}
	ghost := "pf-conc-never-present-item"

	t.Run("并发抢占恰好一次", func(t *testing.T) {
		obs := dialPrefillClient(t, url)
		key := bloomTestKey("pf-conc4")
		stateKey, failnKey := prefillStateKeys(key)
		countKey := key + ":__runs"
		prefillCleanupKeys(t, obs, key, stateKey, failnKey, countKey)

		// 冷启动前提：状态键/计数键缺失（随机键本应不存在，显式 Del 兜底）
		if err := obs.Del(context.Background(), stateKey, failnKey, countKey).Err(); err != nil {
			t.Fatalf("预清理: %v", err)
		}

		// fn 慢 200ms 撑大 building 窗口，确保败者齐射必然撞上持锁态
		newFn := func(rc *redisClient) PrefillFunc {
			return func(ctx context.Context, ingest PrefillIngest) error {
				pIncr(t, rc, countKey)
				time.Sleep(200 * time.Millisecond)
				_, err := ingest.AddMulti(ctx, seeds...)
				return err
			}
		}
		// 不 stopLoop：败者 acquire=0 静默返回后不就地改相位（设计如此），
		// 其本地收敛唯一路径是 loop syncOnce；同时同刻热路径 Exists 制造
		// 真并发抢锁（各实例初值 Uninitialized，首个 tick 前毫秒级齐射）。
		insts := make([]*prefillFilter, nInst)
		for i := range insts {
			rc := dialPrefillClient(t, url)
			insts[i] = newBitmapPrefillReal(t, rc, key, newFn(rc),
				WithSyncInterval(syncI), WithRebuildTimeout(10*time.Second))
		}

		// 齐射：4 实例同刻热路径 Exists → triggerLazy → run(force=0) 并发抢锁
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, pf := range insts {
			wg.Add(1)
			go func(pf *prefillFilter) {
				defer wg.Done()
				<-start
				_, _ = pf.Exists(context.Background(), ghost)
			}(pf)
		}
		close(start)
		wg.Wait()

		// 权威：恰一个赢家完成 → ready；Redis 侧计数恰好 1（fn 全集群
		// 只执行一次；ready 后 force=0 不可再抢，状态机静态，单读即可）
		waitAuthVal(t, obs, stateKey, pvReady, prefillITTimeout)
		waitRunsCount(t, obs, countKey, 1, 2*time.Second)

		// 败者静默不排队：每实例 acquire 尝试有界（热路径齐射 + 收敛前
		// ≤2 个 tick 的有限空转尝试；真排队/重试风暴会远超此上界）
		for i, pf := range insts {
			if n := pf.coord.acquireTries.Load(); n > 8 {
				t.Fatalf("实例 %d acquire 尝试 %d 次，疑似排队/重试风暴（上界 8）", i, n)
			}
		}

		// 全部实例本地收敛 Ready 且真实查询：命中回灌数据 + absent=false
		// （false 判别证明已过降级期，非恒 true 蒙混）
		for i, pf := range insts {
			waitReadyFresh(t, pf.coord, prefillITTimeout)
			for _, s := range seeds {
				if ok, err := pf.Exists(context.Background(), s); err != nil || !ok {
					t.Fatalf("实例 %d 真实查询应命中回灌项 %v: ok=%v err=%v", i, s, ok, err)
				}
			}
			if ok, err := pf.Exists(context.Background(), ghost); err != nil || ok {
				t.Fatalf("实例 %d 新鲜 Ready 下 absent 应真实 false（降级未退出？): ok=%v err=%v", i, ok, err)
			}
		}
	})

	t.Run("退避齐射仅一个新执行者", func(t *testing.T) {
		obs := dialPrefillClient(t, url)
		key := bloomTestKey("pf-volley")
		stateKey, failnKey := prefillStateKeys(key)
		countKey := key + ":__runs"
		prefillCleanupKeys(t, obs, key, stateKey, failnKey, countKey)
		if err := obs.Del(context.Background(), stateKey, failnKey, countKey).Err(); err != nil {
			t.Fatalf("预清理: %v", err)
		}

		const retryInitial = 400 * time.Millisecond
		// fn：Redis 侧计数决定首轮失败/后续成功——首次执行（n==1）失败，
		// 退避到期后齐射的新一轮成功回灌
		newFn := func(rc *redisClient) PrefillFunc {
			return func(ctx context.Context, ingest PrefillIngest) error {
				pIncr(t, rc, countKey)
				// INCR 与判值分离：读当前计数以判首轮。INCR 已发生于上
				// 一行，读回值即本实例的名次（并发下唯一，因为整个集群
				// 同一时刻至多一个 fn 执行者——acquire 持锁保证）。
				if v := pGET(t, rc, countKey); v == "1" {
					return errors.New("prefill volley boom (round 1)")
				}
				time.Sleep(200 * time.Millisecond)
				_, err := ingest.AddMulti(ctx, seeds...)
				return err
			}
		}
		insts := make([]*prefillFilter, nInst)
		for i := range insts {
			rc := dialPrefillClient(t, url)
			// 不 stopLoop：ticker 链（syncOnce→triggerLazy→run force=0）
			// 自然齐射——fail TTL 到期后各实例 ≤1 tick 同步到 Uninitialized
			// 并抢占，验证权威 acquire 只放行一个新执行者
			insts[i] = newBitmapPrefillReal(t, rc, key, newFn(rc),
				WithSyncInterval(syncI), WithRebuildTimeout(10*time.Second),
				WithRetryBackoff(retryInitial, 5*time.Second))
		}

		// 首轮失败记账：failn 增长至 1（Redis 侧权威计数）且权威状态=fail
		// 带 PTTL∈(0, backoff(1)]——failn 键 24h 不过期，轮询到 failn=1 的
		// 任一时刻若 fail 仍在 400ms 退避窗内，两条件可同拍命中；单一权威
		// 键对所有实例同一，即"fail TTL 全局一致"。
		var failMax time.Duration
		prefillEventually(t, "权威 fail 相位 + failn=1 + PTTL∈backoff(1)", prefillITTimeout, func() (bool, string) {
			v := pGET(t, obs, stateKey)
			n := pGET(t, obs, failnKey)
			if v != pvFail || n != "1" {
				return false, fmt.Sprintf("state=%q failn=%q", v, n)
			}
			pttl := obs.PTTL(context.Background(), stateKey).Val()
			if pttl > failMax {
				failMax = pttl
			}
			return pttl > 0 && pttl <= retryInitial+150*time.Millisecond,
				fmt.Sprintf("state=%q failn=%q pttl=%v", v, n, pttl)
		})
		t.Logf("fail PTTL 观测峰值=%v（backoff(1)=%v）", failMax, retryInitial)

		// 退避到期 → 键缺失 → ticker 齐射 → 仅一个赢家成功 → ready；
		// Redis 侧执行计数封顶 2（1 败 + 1 胜，无第二个新执行者）
		waitAuthVal(t, obs, stateKey, pvReady, prefillITTimeout)
		waitRunsCount(t, obs, countKey, 2, 3*time.Second)

		for i, pf := range insts {
			waitReadyFresh(t, pf.coord, prefillITTimeout)
			for _, s := range seeds {
				if ok, err := pf.Exists(context.Background(), s); err != nil || !ok {
					t.Fatalf("恢复后实例 %d 应命中回灌项 %v: ok=%v err=%v", i, s, ok, err)
				}
			}
		}
	})
}

// --- 用例 2：重建期混合读写流量（降级正确性） ---

// TestPrefillConcurrencyMixedReadWrite 实例 A 执行慢回灌（fn sleep≈500ms
// + 分批 Add），实例 B 全程并发 Exists/Add 负载（每 10ms 一轮）：B 在
// 重建窗口内 Exists 无假阴（恒 true 或真实命中）、Add 放行无错误、
// Building 期不发抢锁风暴（acquire 失败静默）、A 完成后 B ≤2 tick
// 收敛新鲜 Ready 且命中 A 回灌的全部样本。
func TestPrefillConcurrencyMixedReadWrite(t *testing.T) {
	url := requirePrefillRedis(t)
	obs := dialPrefillClient(t, url)
	const syncI = 50 * time.Millisecond

	key := bloomTestKey("pf-mixrw")
	stateKey, failnKey := prefillStateKeys(key)
	countKey := key + ":__runs"
	prefillCleanupKeys(t, obs, key, stateKey, failnKey, countKey)

	seeds := make([]any, 20)
	for i := range seeds {
		seeds[i] = fmt.Sprintf("mixrw-seed-%d", i)
	}

	// A 的回灌函数：慢速 + 分批（4 批 ×125ms ≈ 500ms 重建窗口）
	rcA := dialPrefillClient(t, url)
	fnA := func(ctx context.Context, ingest PrefillIngest) error {
		pIncr(t, rcA, countKey)
		const batches = 4
		per := len(seeds) / batches
		for b := range batches {
			if _, err := ingest.AddMulti(ctx, seeds[b*per:(b+1)*per]...); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(125 * time.Millisecond):
			}
		}
		return nil
	}

	// 前置：冷态清干净 → 权威置 ready（force=0 永不抢占 Ready，B 热路径
	// triggerLazy 对 Ready/Building 均短路——重建窗口的抢锁静默前提成立）
	if err := obs.Del(context.Background(), stateKey, failnKey, countKey).Err(); err != nil {
		t.Fatalf("预清理: %v", err)
	}
	pfA := newBitmapPrefillReal(t, rcA, key, fnA,
		WithSyncInterval(syncI), WithRebuildTimeout(10*time.Second))
	pfA.coord.stopLoop() // A 只走显式 Reset（force=1），排除 ticker 干扰
	if err := obs.Set(context.Background(), stateKey, pvReady, 0).Err(); err != nil {
		t.Fatalf("预置 ready: %v", err)
	}
	// 种子数据落盘（窗口开始前数据真实在场且可回灌复原）
	if _, err := pfA.AddMulti(context.Background(), seeds...); err != nil {
		t.Fatalf("预置数据: %v", err)
	}

	// B：loop 全程在线（tick 收敛能力是被测语义）
	rcB := dialPrefillClient(t, url)
	pfB := newBitmapPrefillReal(t, rcB, key, fnA, WithSyncInterval(syncI), WithRebuildTimeout(10*time.Second))
	waitReadyFresh(t, pfB.coord, prefillITTimeout) // 窗口前 B 已新鲜 Ready（真实查询态）
	for _, s := range seeds {
		if ok, err := pfB.Exists(context.Background(), s); err != nil || !ok {
			t.Fatalf("窗口前 B 真实查询应命中 %v: ok=%v err=%v", s, ok, err)
		}
	}
	baseTries := pfB.coord.acquireTries.Load()

	// B 混合负载：每 10ms 一轮 Exists(seed)+Add；Exists=false（降级期
	// 不该出现、回灌中的数据不该查无）或 Add 报错即计数
	var rounds, falseNeg, addErrs atomic.Int64
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		ctx := context.Background()
		for i := int64(0); ; i++ {
			select {
			case <-stopCh:
				return
			default:
			}
			s := seeds[i%int64(len(seeds))]
			if ok, err := pfB.Exists(ctx, s); err != nil || !ok {
				falseNeg.Add(1)
			}
			if _, err := pfB.Add(ctx, fmt.Sprintf("mixrw-load-%d", i)); err != nil {
				addErrs.Add(1)
			}
			rounds.Add(1)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	prefillEventually(t, "B 混合负载启动", 3*time.Second, func() (bool, string) {
		n := rounds.Load()
		return n >= 1, fmt.Sprintf("rounds=%d", n)
	})

	// A 执行慢重建（同步至完成）：acquire → Δ → 清空 → 分批回灌 → ready
	if err := pfA.Reset(context.Background()); err != nil {
		t.Fatalf("A.Reset（force 重建）: %v", err)
	}
	if p, err := pfA.State(context.Background()); err != nil || p != PrefillReady {
		t.Fatalf("A 完成后权威应 ready: got (%v, %v)", p, err)
	}

	// A 完成后 B ≤2 tick 收敛新鲜 Ready（100ms + 抖动/RTT 裕量）
	waitReadyFresh(t, pfB.coord, 2*syncI+350*time.Millisecond)
	close(stopCh)
	<-doneCh

	if n := rounds.Load(); n < 20 {
		t.Fatalf("B 负载轮数 %d 过少，重建窗口（≥500ms）未被充分覆盖", n)
	}
	if n := falseNeg.Load(); n != 0 {
		t.Fatalf("重建窗口内 B Exists 出现 %d 次假阴（降级正确性破坏）", n)
	}
	if n := addErrs.Load(); n != 0 {
		t.Fatalf("重建窗口内 B Add 出现 %d 次错误（应恒放行）", n)
	}
	// Building 期无抢锁风暴：Δ 传播保证 B 见 building 前 phase 短路生效，
	// 理论增量 0；容忍 1 次边界（B 启动初期的一次性惰性触发）
	if n := pfB.coord.acquireTries.Load() - baseTries; n > 1 {
		t.Fatalf("B 在重建窗口发起 %d 次 acquire（>1 即抢锁风暴）", n)
	}
	// B 命中 A 回灌的全部样本（新鲜 Ready 真实查询）
	for _, s := range seeds {
		prefillEventually(t, fmt.Sprintf("B 命中回灌样本 %v", s), 2*time.Second, func() (bool, string) {
			ok, err := pfB.Exists(context.Background(), s)
			return ok && err == nil, fmt.Sprintf("%v: ok=%v err=%v", s, ok, err)
		})
	}
}

// --- 用例 3：三机制串联生命周期（自动预填 → Reset → 探测自愈） ---

// TestPrefillLifecycleChainProbe 单实例顺序串演完整链路并逐步断言相位/
// 状态键：新建（缺失→降级→ticker 自动预填→ready）→ 正常服务真实查询
// → Reset（force 重灌→building→ready、回灌计数 +1）→ 手工 DEL 数据键
// 模拟数据丢失 + 权威仍 ready → 探测（肯定样本查不到→force 重建→ready
// 且样本恢复命中）。fn 恒 sleep 300ms（>probeInterval 50ms）：探测触发
// 的重建若被探测 ctx 预算截断则永远到不了 ready——链路成功即"重建预算
// 独立于 interval（方案 A）"经真实慢 fn 的顺带验证。
func TestPrefillLifecycleChainProbe(t *testing.T) {
	url := requirePrefillRedis(t)
	rc := dialPrefillClient(t, url)
	const syncI = 50 * time.Millisecond
	const fnSleep = 300 * time.Millisecond // > probeInterval：预算独立性验证锚

	key := bloomTestKey("pf-life")
	stateKey, failnKey := prefillStateKeys(key)
	countKey := key + ":__runs"
	prefillCleanupKeys(t, rc, key, stateKey, failnKey, countKey)
	ghost := "pf-life-never-present"

	seeds := make([]any, 5)
	for i := range seeds {
		seeds[i] = fmt.Sprintf("life-seed-%d", i)
	}
	fn := func(ctx context.Context, ingest PrefillIngest) error {
		pIncr(t, rc, countKey)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(fnSleep):
		}
		_, err := ingest.AddMulti(ctx, seeds...)
		return err
	}

	if err := rc.Del(context.Background(), stateKey, failnKey, countKey).Err(); err != nil {
		t.Fatalf("预清理: %v", err)
	}
	pf := newBitmapPrefillReal(t, rc, key, fn,
		WithSyncInterval(syncI), WithRebuildTimeout(10*time.Second),
		WithLivenessProbe(50*time.Millisecond, func(context.Context) ([]any, error) {
			cp := make([]any, len(seeds))
			copy(cp, seeds)
			return cp, nil
		}))
	co := pf.coord

	// 相位 1：新建即降级（确定性断言，无竞态：本地初值 Uninitialized、
	// updatedAt 零值 → 门控恒 true；inner 真实查询 false 对照）
	if s := co.loadLocal(); s.phase != PrefillUninitialized || !s.updatedAt.IsZero() {
		t.Fatalf("构造后本地应 Uninitialized+零时间: %+v", s)
	}
	if ok, err := pf.Exists(context.Background(), ghost); err != nil || !ok {
		t.Fatalf("降级期 Exists 应恒 true: ok=%v err=%v", ok, err)
	}
	if ok, err := pf.inner.Exists(context.Background(), ghost); err != nil || ok {
		t.Fatalf("inner 真实查询对照应 false: ok=%v err=%v", ok, err)
	}
	// 状态键缺失 → State 权威口径 Uninitialized
	if p, err := pf.State(context.Background()); err != nil || p != PrefillUninitialized {
		t.Fatalf("预清干净后 State 应 Uninitialized: got (%v, %v)", p, err)
	}

	// ticker 自动预填链（无人工 Reset）→ ready；Redis 侧计数=1
	waitAuthVal(t, rc, stateKey, pvReady, prefillITTimeout)
	waitRunsCount(t, rc, countKey, 1, 2*time.Second)
	waitReadyFresh(t, co, prefillITTimeout)

	// 相位 2：正常服务真实查询（命中种子 + absent=false + State/本地一致）
	if p, err := pf.State(context.Background()); err != nil || p != PrefillReady {
		t.Fatalf("自动预填后 State 应 Ready: got (%v, %v)", p, err)
	}
	for _, s := range seeds {
		if ok, err := pf.Exists(context.Background(), s); err != nil || !ok {
			t.Fatalf("服务期应命中回灌项 %v: ok=%v err=%v", s, ok, err)
		}
	}
	if ok, err := pf.Exists(context.Background(), ghost); err != nil || ok {
		t.Fatalf("服务期 absent 应真实 false: ok=%v err=%v", ok, err)
	}

	// 相位 3：Reset force 重灌——building 瞬态由采样器捕获，计数 +1
	sampler3 := startAuthValueSampler(t, rc, stateKey)
	if err := pf.Reset(context.Background()); err != nil {
		t.Fatalf("Reset（force 重建）: %v", err)
	}
	prefillEventually(t, "Reset 期间观测到权威 building", 2*time.Second, func() (bool, string) {
		seen, _ := sampler3.snapshot()
		return seen[pvBuilding] > 0, fmt.Sprintf("采样=%v", seen)
	})
	waitRunsCount(t, rc, countKey, 2, 2*time.Second)
	if p, err := pf.State(context.Background()); err != nil || p != PrefillReady {
		t.Fatalf("Reset 后 State 应 Ready: got (%v, %v)", p, err)
	}
	waitReadyFresh(t, co, prefillITTimeout)
	for _, s := range seeds {
		if ok, err := pf.Exists(context.Background(), s); err != nil || !ok {
			t.Fatalf("Reset 重灌后应命中 %v: ok=%v err=%v", s, ok, err)
		}
	}

	// 相位 4：手工 DEL 数据键模拟数据丢失；权威键（ready 无 TTL）不动。
	// DEL 返回后同步断言丢失现场——此时探测重建（≤50ms 触发 + Δ + fn
	// 300ms）尚不可能完成，inner 必然全 false
	if err := rc.Del(context.Background(), key).Err(); err != nil {
		t.Fatalf("Del 数据键: %v", err)
	}
	if ok, err := pf.inner.Exists(context.Background(), seeds[0]); err != nil || ok {
		t.Fatalf("数据丢失现场 inner 应 false: ok=%v err=%v", ok, err)
	}
	if p, err := pf.State(context.Background()); err != nil || p != PrefillReady {
		t.Fatalf("数据丢失后权威仍应 Ready（状态键未动）: got (%v, %v)", p, err)
	}

	// 相位 5：探测自愈——probeFn 肯定样本查不到 → run(force=1) →
	// building →（慢 fn 300ms，不受 probeInterval 50ms 截断）→ ready
	sampler5 := startAuthValueSampler(t, rc, stateKey)
	waitRunsCount(t, rc, countKey, 3, prefillITTimeout)
	prefillEventually(t, "探测触发重建观测到 building", 2*time.Second, func() (bool, string) {
		seen, _ := sampler5.snapshot()
		return seen[pvBuilding] > 0, fmt.Sprintf("采样=%v", seen)
	})
	waitAuthVal(t, rc, stateKey, pvReady, 3*time.Second)
	waitReadyFresh(t, co, prefillITTimeout)
	if p, err := pf.State(context.Background()); err != nil || p != PrefillReady {
		t.Fatalf("探测自愈后 State 应 Ready: got (%v, %v)", p, err)
	}
	// 样本恢复命中（门面真实查询 + inner 直查双口径）
	for _, s := range seeds {
		prefillEventually(t, fmt.Sprintf("探测自愈后恢复命中 %v", s), 3*time.Second, func() (bool, string) {
			ok, err := pf.Exists(context.Background(), s)
			return ok && err == nil, fmt.Sprintf("%v: ok=%v err=%v", s, ok, err)
		})
	}
	// 计数封顶 3（自动 1 + Reset 1 + 探测 1）：探测预算未被 interval
	// 截断（截断则 fn 永远跑不完 → 计数不会到 3 并停在 fail/building 循环）
	waitRunsCount(t, rc, countKey, 3, 3*time.Second)
}

// --- 用例 4：bfCmdImpl（BF.* 模块）真机 prefill/探测 ---

// TestPrefillBfCmdPrefillProbe 真 Redis + RedisBloom 模块（Capability().
// Probe 后 HasBloom 为真才跑，无模块 t.Skip 如实报告）：工厂 auto 分派
// bfCmdImpl，白盒断言实现类型；N=2 实例并发抢占（用例 1 缩微版）——fn
// Redis 侧计数=1、收敛 ready；再手工 DEL 数据键，经 WithLivenessProbe
// 肯定样本失效触发一次 force 重建（BF.RESERVE 清空重建 + 回灌配合，
// 计数=2、样本恢复命中、BF.CARD>0）。与用例 1-3 的 bitmap 路径互补。
func TestPrefillBfCmdPrefillProbe(t *testing.T) {
	url := requirePrefillRedis(t)
	obs := dialPrefillClient(t, url)
	ctx := context.Background()
	if err := obs.Capability().Probe(ctx); err != nil {
		t.Fatalf("Capability Probe: %v", err)
	}
	if !obs.Capability().HasBloom() {
		t.Skip("server has no bf module; skip bfCmdImpl prefill/probe test")
	}

	const (
		syncI  = 50 * time.Millisecond
		nInst  = 2
		fnSlow = 150 * time.Millisecond // 撑大 building 窗口，保证双实例齐射撞锁
	)
	key := bloomTestKey("pf-bfcmd")
	stateKey, failnKey := prefillStateKeys(key)
	countKey := key + ":__runs"
	prefillCleanupKeys(t, obs, key, stateKey, failnKey, countKey)

	seeds := make([]any, 3)
	for i := range seeds {
		seeds[i] = fmt.Sprintf("bfcmd-seed-%d", i)
	}
	ghost := "bfcmd-never-present"

	newFn := func(rc *redisClient) PrefillFunc {
		return func(ctx context.Context, ingest PrefillIngest) error {
			pIncr(t, rc, countKey)
			time.Sleep(fnSlow)
			_, err := ingest.AddMulti(ctx, seeds...)
			return err
		}
	}

	if err := obs.Del(ctx, stateKey, failnKey, countKey).Err(); err != nil {
		t.Fatalf("预清理: %v", err)
	}
	insts := make([]*prefillFilter, nInst)
	for i := range insts {
		rc := dialPrefillClient(t, url)
		// 工厂按**各 client 自身**能力缓存分派：每实例构造前显式 Probe
		// （HasBloom 未探测即 false → 会错走 bitmap 路径）
		if err := rc.Capability().Probe(ctx); err != nil {
			t.Fatalf("实例 %d Capability Probe: %v", i, err)
		}
		if !rc.Capability().HasBloom() {
			t.Skipf("实例 %d Probe 后 HasBloom=false（模块中途不可用？）；skip bfCmdImpl test", i)
		}
		pf := newFactoryPrefillReal(t, rc, key, newFn(rc),
			WithSyncInterval(syncI), WithRebuildTimeout(10*time.Second),
			WithLivenessProbe(50*time.Millisecond, func(context.Context) ([]any, error) {
				cp := make([]any, len(seeds))
				copy(cp, seeds)
				return cp, nil
			}))
		// 白盒锁定 BF.* 路径（HasBloom 为真时工厂必选 bfCmdImpl）
		if _, isBF := pf.inner.(*bfCmdImpl); !isBF {
			t.Fatalf("HasBloom 下应分派 bfCmdImpl，got %T", pf.inner)
		}
		// 不 stopLoop：败者本地收敛依赖 syncOnce（同用例 1 教训）；
		// 抢锁并发仍由同刻热路径齐射制造
		insts[i] = pf
	}

	// N=2 同刻热路径齐射 → 恰一赢家
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, pf := range insts {
		wg.Add(1)
		go func(pf *prefillFilter) {
			defer wg.Done()
			<-start
			_, _ = pf.Exists(context.Background(), ghost)
		}(pf)
	}
	close(start)
	wg.Wait()

	waitAuthVal(t, obs, stateKey, pvReady, prefillITTimeout)
	waitRunsCount(t, obs, countKey, 1, 2*time.Second)
	for i, pf := range insts {
		waitReadyFresh(t, pf.coord, prefillITTimeout)
		for _, s := range seeds {
			if ok, err := pf.Exists(context.Background(), s); err != nil || !ok {
				t.Fatalf("BF 实例 %d 应命中回灌项 %v: ok=%v err=%v", i, s, ok, err)
			}
		}
		if ok, err := pf.Exists(context.Background(), ghost); err != nil || ok {
			t.Fatalf("BF 实例 %d 新鲜 Ready 下 absent 应真实 false: ok=%v err=%v", i, ok, err)
		}
	}

	// 探测失效触发一次：DEL 数据键（BF.EXISTS 对缺失键返回 0——真实
	// 失效证据）→ 任一实例 probe force 重建（另一实例撞锁静默）→
	// inner.Reset=DEL+BF.RESERVE 重建 + 回灌配合 → 计数封顶 2、样本恢复
	if err := obs.Del(ctx, key).Err(); err != nil {
		t.Fatalf("Del BF 数据键: %v", err)
	}
	waitRunsCount(t, obs, countKey, 2, prefillITTimeout)
	waitAuthVal(t, obs, stateKey, pvReady, 5*time.Second)
	for i, pf := range insts {
		waitReadyFresh(t, pf.coord, prefillITTimeout)
		for _, s := range seeds {
			prefillEventually(t, fmt.Sprintf("BF 探测自愈后实例 %d 命中 %v", i, s), 3*time.Second, func() (bool, string) {
				ok, err := pf.Exists(context.Background(), s)
				return ok && err == nil, fmt.Sprintf("%v: ok=%v err=%v", s, ok, err)
			})
		}
	}
	// BF.CARD>0：重建键经 BF.RESERVE 真实建立且回灌项落位（模块版
	// Reset 与 prefill 配合的直接证据）
	card, err := insts[0].Card(context.Background())
	if err != nil || card <= 0 {
		t.Fatalf("探测重建后 BF.CARD 应 >0: card=%d err=%v", card, err)
	}
	// 计数稳定封顶 2（ready 后探测健康不再触发；缓冲读一次确认）
	waitRunsCount(t, obs, countKey, 2, 2*time.Second)
}
