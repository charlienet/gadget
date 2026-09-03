package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// --- ledger 基础行为 ---

func TestLedgerGetOrCreateShared(t *testing.T) {
	ld := newLedger()
	now := time.Unix(0, 0)

	e1 := ld.getOrCreate("a", now)
	e2 := ld.getOrCreate("a", now.Add(time.Second))
	if e1 != e2 {
		t.Fatal("同 key 必须复用同一账本条目")
	}
	if ld.getOrCreate("b", now) == e1 {
		t.Fatal("不同 key 必须是独立条目")
	}
	if !e1.idleAt.Equal(now) {
		t.Fatalf("创建时应记录 idleAt，got %v", e1.idleAt)
	}
}

// --- sweeper 闲置回收（fake clock 驱动，sweepOnce 直调保证确定性）---

func newSweeperLimiter(t *testing.T, clock *fakeClock) *Limiter {
	t.Helper()
	fb := &fakeBackend{fn: grantAll}
	l := New(fb, WithClock(clock), WithRate(100, time.Second), WithBurst(1000), WithIdleRetention(60*time.Second))
	t.Cleanup(func() { l.Close() })
	return l
}

func TestSweeperReclaimsIdleEntries(t *testing.T) {
	clock := newFakeClock()
	l := newSweeperLimiter(t, clock)

	ctx := context.Background()
	if _, err := l.Allow(ctx, "a", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Allow(ctx, "b", 1); err != nil {
		t.Fatal(err)
	}
	if n := len(l.ledger.entries); n != 2 {
		t.Fatalf("应有 2 个账本条目，got %d", n)
	}

	// "b" 保持闲置 61s；"a" 在回收前刚被访问（idleAt 刷新）——
	// 验证"持锁复检 idleAt：宁可延后一轮"（L5）。
	clock.Advance(61 * time.Second)
	entA := l.ledger.getOrCreate("a", clock.Now())
	entA.mu.Lock()
	entA.idleAt = clock.Now()
	entA.mu.Unlock()

	l.sweepOnce(clock.Now())

	if _, ok := l.ledger.entries["a"]; !ok {
		t.Fatal("刚被触碰的条目不应删除（持锁复检）")
	}
	if _, ok := l.ledger.entries["b"]; ok {
		t.Fatal("闲置超阈条目应被回收")
	}

	// 回收后重新 Allow 应重建条目并可再次批发（账本不悬空）。
	if ok, err := l.Allow(ctx, "b", 1); !ok || err != nil {
		t.Fatalf("回收后 Allow(b) 应正常放行: %v %v", ok, err)
	}
}

func TestSweeperSkipsInFlightWholesale(t *testing.T) {
	clock := newFakeClock()
	l := newSweeperLimiter(t, clock)

	ent := l.ledger.getOrCreate("k", clock.Now())
	ent.mu.Lock()
	ent.pending = &pendingBatch{done: make(chan struct{})} // 模拟批发在途
	ent.mu.Unlock()

	clock.Advance(2 * time.Minute) // idleAt 早已超阈
	l.sweepOnce(clock.Now())

	if _, ok := l.ledger.entries["k"]; !ok {
		t.Fatal("批发在途的条目不得删除（否则 leader 结算悬空）")
	}
}

func TestSweeperConcurrentWithAllow(t *testing.T) {
	// 并发压测 sweeper 与 Allow 的锁交互（-race 守卫：map 读写与条目复检）。
	clock := newFakeClock()
	l := newSweeperLimiter(t, clock)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			clock.Advance(30 * time.Second)
			l.sweepOnce(clock.Now())
		}
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = l.Allow(context.Background(), "hot", 1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// --- Backend panic 中断批发（N-A）：followers 不死等、key 可恢复、panic 不吞 ---

func TestBackendPanicAbortsBatchFollowersSurvive(t *testing.T) {
	clock := newFakeClock()
	resume := make(chan struct{})
	entered := make(chan struct{}, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	defer release()

	var calls atomicInt
	fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		if calls.add(1) == 1 {
			entered <- struct{}{} // leader 已进入批发
			<-resume              // 等 followers 挂上
			panic("boom: backend exploded")
		}
		return 5, 0, nil
	}}
	l := New(fb, WithClock(clock), WithRate(100, time.Second), WithBurst(1000))
	defer l.Close()

	// leader：panic 必须穿透到它的调用方 goroutine（本包不 recover）。
	leaderDone := make(chan any, 1)
	go func() {
		defer func() { leaderDone <- recover() }()
		_, _ = l.Allow(context.Background(), "k", 1)
		leaderDone <- nil
	}()
	<-entered

	// follower：挂在共享 done 上等 leader 的广播。
	followerErr := make(chan error, 1)
	go func() {
		_, err := l.Allow(context.Background(), "k", 1)
		followerErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	release() // 放行 leader 去 panic

	if r := <-leaderDone; r == nil {
		t.Fatal("Backend panic 必须继续穿透，不得被 recover 吞掉")
	}

	select {
	case err := <-followerErr:
		if !errors.Is(err, errBrokenBatch) {
			t.Fatalf("follower 应收到批发中断错误，got %v", err)
		}
		// 经"命令级错误原样透传"分诊：不进 FailPolicy 的 Unavailable 通道。
		if errors.Is(err, ErrBackendUnavailable) || errors.Is(err, ErrFailOpen) {
			t.Fatalf("中断错误不得混入兜底通道，got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower 永久挂死：panic 路径未广播 done（N-A 防护失效）")
	}

	// key 必须恢复：pending 已清理，下一次 Allow 重新批发走正常授予。
	ok, err := l.Allow(context.Background(), "k", 1)
	if !ok || err != nil {
		t.Fatalf("panic 清理后该 key 应恢复正常批发: %v %v", ok, err)
	}
}

// atomicInt 最小自增计数（测试专用）。
type atomicInt struct {
	mu sync.Mutex
	n  int
}

func (a *atomicInt) add(d int) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n += d
	return a.n
}

// --- 租约账本纯存量补充守卫：FailOpen 放行不得注入/扣减存量 ---

func TestFailOpenDoesNotTouchLedger(t *testing.T) {
	clock := newFakeClock()
	fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		return 0, 0, fmt.Errorf("%w: stub down", ErrBackendUnavailable)
	}}
	l := New(fb, WithClock(clock), WithRate(10, time.Second), WithBurst(10), WithFailPolicy(FailOpen))
	defer l.Close()

	for i := 0; i < 5; i++ {
		ok, err := l.Allow(context.Background(), "k", 1)
		if !ok || err == nil {
			t.Fatalf("FailOpen 应放行带错: %v %v", ok, err)
		}
	}
	ent := l.ledger.getOrCreate("k", clock.Now())
	ent.mu.Lock()
	remain := ent.remain
	ent.mu.Unlock()
	if remain != 0 {
		t.Fatalf("FailOpen 兜底不得向账本注入存量，got remain=%d", remain)
	}
}

// --- 批发失败后静默期不误置 ---

func TestWholesaleErrorLeavesNoSilence(t *testing.T) {
	clock := newFakeClock()
	unavailable := &atomicBool{}
	fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		if unavailable.isSet() {
			return 0, 0, fmt.Errorf("%w: stub down", ErrBackendUnavailable)
		}
		return 5, 0, nil
	}}
	l := New(fb, WithClock(clock), WithRate(10, time.Second), WithBurst(10), WithFailPolicy(FailClosed))
	defer l.Close()

	unavailable.set(true)
	if _, err := l.Allow(context.Background(), "k", 1); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("应透传不可用错误（Closed 拒绝），got %v", err)
	}
	// 错误路径不得设置静默期：恢复后下一次 Allow 应立刻重新批发。
	unavailable.set(false)
	if ok, err := l.Allow(context.Background(), "k", 1); !ok || err != nil {
		t.Fatalf("后端恢复应立即放行: %v %v", ok, err)
	}
}

// atomicBool 是最小 bool 开关（测试专用）。
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomicBool) set(v bool)  { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicBool) isSet() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.v }
