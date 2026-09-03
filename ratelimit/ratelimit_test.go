package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- 测试基础设施：可控时钟与 fake 后端 ---

// fakeClock 是可控时间源：Advance 推进时间并触发到期的 after 通道。
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	fire time.Time
	c    chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 推进 now 并唤醒所有到期计时器。
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	live := c.timers[:0]
	for _, t := range c.timers {
		if !t.fire.After(c.now) {
			t.c <- c.now // cap 1 缓冲，无人消费也不阻塞
		} else {
			live = append(live, t)
		}
	}
	c.timers = live
}

// after 是注入 Limiter.after 字段的可控计时器（对齐 time.After 签名）。
func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, &fakeTimer{fire: c.now.Add(d), c: ch})
	return ch
}

// fakeBackend 是可编排的 Backend：计数调用、按 fn 编排结果、可阻塞。
type fakeBackend struct {
	mu      sync.Mutex
	calls   int
	lastKey string
	lastMax int // 历史最大 want
	gotMode []GrantMode

	fn func(ctx context.Context, key string, want int, spec Spec, mode GrantMode) (int, time.Duration, error)
}

func (f *fakeBackend) Wholesale(ctx context.Context, key string, want int, spec Spec, mode GrantMode) (int, time.Duration, error) {
	f.mu.Lock()
	f.calls++
	f.lastKey = key
	if want > f.lastMax {
		f.lastMax = want
	}
	f.gotMode = append(f.gotMode, mode)
	fn := f.fn
	f.mu.Unlock()

	if fn == nil {
		return 0, 0, nil
	}
	return fn(ctx, key, want, spec, mode)
}

func (f *fakeBackend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeBackend) modes() []GrantMode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]GrantMode(nil), f.gotMode...)
}

// grantAll 是"全额授予"的 fn。
func grantAll(_ context.Context, _ string, want int, _ Spec, _ GrantMode) (int, time.Duration, error) {
	return want, 0, nil
}

// --- New / Option ---

func TestNewNilBackendPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(nil) 必须 panic")
		}
	}()
	New(nil)
}

func TestOptionsDefaults(t *testing.T) {
	l := New(Memory())
	defer l.Close()

	if l.spec.Rate != 100 || l.spec.Per != time.Second {
		t.Fatalf("默认速率应为 100/1s，got %d/%v", l.spec.Rate, l.spec.Per)
	}
	if l.spec.Burst != 200 {
		t.Fatalf("默认 Burst 应为 2×Rate=200，got %d", l.spec.Burst)
	}
	if l.maxWait != 30*time.Second || l.leaseInterval != time.Second || l.leaseRatio != 0.5 {
		t.Fatalf("默认 maxWait/leaseInterval/leaseRatio 异常: %v %v %v", l.maxWait, l.leaseInterval, l.leaseRatio)
	}
	if l.backendTimeout != time.Second {
		t.Fatalf("默认 backendTimeout 应为 min(1s,5s)=1s，got %v", l.backendTimeout)
	}
	if l.spec.IdleRetention != 60*time.Second {
		t.Fatalf("默认 idleRetention 应为 60s，got %v", l.spec.IdleRetention)
	}
	if l.policy != FailOpen || !l.localLease {
		t.Fatalf("默认应 FailOpen + 租约模式，got %v/%v", l.policy, l.localLease)
	}
}

func TestOptionsDefensiveIgnore(t *testing.T) {
	l := New(Memory(),
		WithRate(0, -time.Second),
		WithBurst(-1),
		WithMaxWait(0),
		WithLeaseInterval(-1),
		WithLeaseRatio(0),
		WithLeaseRatio(1.5),
		WithBackendTimeout(0),
		WithIdleRetention(-1),
		WithLogger(nil),
		WithClock(nil),
	)
	defer l.Close()

	if l.spec.Rate != 100 || l.spec.Per != time.Second {
		t.Fatalf("非法 WithRate 应被忽略，got %d/%v", l.spec.Rate, l.spec.Per)
	}
	if l.spec.Burst != 200 {
		t.Fatalf("非法 WithBurst 应被忽略（保持 2×Rate），got %d", l.spec.Burst)
	}
	if l.maxWait != 30*time.Second || l.leaseInterval != time.Second || l.leaseRatio != 0.5 {
		t.Fatalf("非法时长/比例 Option 应被忽略: %v %v %v", l.maxWait, l.leaseInterval, l.leaseRatio)
	}
	if l.backendTimeout != time.Second || l.spec.IdleRetention != 60*time.Second {
		t.Fatalf("非法 timeout/retention 应被忽略: %v %v", l.backendTimeout, l.spec.IdleRetention)
	}
	if _, ok := l.clock.(systemClock); !ok {
		t.Fatalf("WithClock(nil) 应保持系统时钟，got %T", l.clock)
	}
	if l.logger == nil {
		t.Fatal("WithLogger(nil) 应保持 slog.Default")
	}
}

func TestOptionsBurstFollowsFinalRate(t *testing.T) {
	l := New(Memory(), WithRate(50, 2*time.Second), WithBackendTimeout(3*time.Second), WithLeaseInterval(10*time.Second))
	defer l.Close()

	if l.spec.Rate != 50 || l.spec.Per != 2*time.Second {
		t.Fatalf("WithRate 未生效: %d/%v", l.spec.Rate, l.spec.Per)
	}
	if l.spec.Burst != 100 {
		t.Fatalf("未显式 WithBurst 时应为最终 Rate 的 2 倍 = 100，got %d", l.spec.Burst)
	}
	if l.backendTimeout != 3*time.Second {
		t.Fatalf("WithBackendTimeout 应覆盖推导默认，got %v", l.backendTimeout)
	}
}

func TestWantFormula(t *testing.T) {
	cases := []struct {
		name     string
		opts     []Option
		expected int
	}{
		{"默认 100/1s ratio0.5", nil, 50},
		{"低速率钳制到 1", []Option{WithRate(1, time.Second)}, 1},
		{"超容量钳制到 Burst", []Option{WithRate(1000, time.Second), WithBurst(50)}, 50},
		{"ratio=1 全额批量", []Option{WithRate(20, time.Second), WithLeaseRatio(1)}, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New(Memory(), tc.opts...)
			defer l.Close()
			if got := l.want(); got != tc.expected {
				t.Fatalf("want() = %d，期望 %d", got, tc.expected)
			}
		})
	}
}

// --- H1 速率回归守卫：本地账本纯存量、无自补充 ---

func TestH1RateRegression(t *testing.T) {
	clock := newFakeClock()
	mem := newMemoryBackend(clock)
	l := New(mem,
		WithClock(clock),
		WithRate(100, time.Second),
		WithBurst(100),
	)
	defer l.Close()

	// drain 在当前时钟下反复 Allow 直到被拒，返回放行总数。
	drain := func() int {
		passed := 0
		for {
			ok, err := l.Allow(context.Background(), "k", 1)
			if !ok {
				if !errors.Is(err, ErrExceeded) {
					t.Fatalf("非超限错误: %v", err)
				}
				return passed
			}
			if err != nil {
				t.Fatalf("放行却带错误: %v", err)
			}
			passed++
			if passed > 1000 {
				t.Fatal("放行量失控，本地账本疑似按速率自补充（H1 双重发币）")
			}
		}
	}

	// 静止时钟：桶初始满 100 且无补充，总放行量必须恰等于桶理论量。
	if got := drain(); got != 100 {
		t.Fatalf("静止时钟放行量应为 100（桶理论量），got %d", got)
	}
	// 推进 1s：桶按速率回补 100，再放行 100（速率语义只来自后端桶）。
	clock.Advance(time.Second)
	if got := drain(); got != 100 {
		t.Fatalf("推进 1s 后放行量应为 100，got %d", got)
	}
	// 推进 5s：回补封顶 Burst=100，放行量仍恰为 100（桶封顶生效）。
	clock.Advance(5 * time.Second)
	if got := drain(); got != 100 {
		t.Fatalf("推进 5s 后放行量应封顶 100，got %d", got)
	}
}

// --- 批发合并（N1 锁纪律 / followers 共享 / leader ctx 隔离）---

func TestWholesaleMergeFollowersShare(t *testing.T) {
	fb := &fakeBackend{fn: grantAll}
	l := New(fb, WithRate(300, time.Second), WithBurst(1000))
	defer l.Close()

	const goroutines = 100
	var wg sync.WaitGroup
	passed := atomic.Int64{}
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ok, err := l.Allow(context.Background(), "hot", 1)
			if ok && err == nil {
				passed.Add(1)
			}
		}()
	}
	wg.Wait()

	// 首批 want=150 ≥ 100：全部请求或共享 leader 结果、或命中注入后的
	// 存量，批发次数必须恰为 1（in-flight 合并生效）。
	if got := passed.Load(); got != goroutines {
		t.Fatalf("全部 %d 个请求应放行，got %d", goroutines, got)
	}
	if calls := fb.count(); calls != 1 {
		t.Fatalf("100 并发冷启动应合并为 1 次批发，got %d", calls)
	}
}

func TestN1HotPathNotBlockedDuringWholesale(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	fb := &fakeBackend{fn: func(ctx context.Context, key string, want int, _ Spec, _ GrantMode) (int, time.Duration, error) {
		if key == "a" {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-gate // 模拟远端慢响应（阻塞期间 ent.mu 必须已释放）
		}
		return want, 0, nil
	}}
	l := New(fb, WithRate(100, time.Second), WithBurst(1000))
	defer l.Close()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	defer release()

	// 触发 key "a" 的在途批发（leader 阻塞在 gate 上）。
	go func() { _, _ = l.Allow(context.Background(), "a", 500) }()
	<-entered

	// 预置 a 的存量（模拟此前批发注入），再验证热路径不被在途批发阻塞。
	ent := l.ledger.getOrCreate("a", l.clock.Now())
	ent.mu.Lock()
	ent.remain = 100
	ent.mu.Unlock()

	done := make(chan [2]bool, 2)
	go func() {
		ok, _ := l.Allow(context.Background(), "a", 10) // 同 key 存量充足的热路径
		done <- [2]bool{ok, true}
	}()
	go func() {
		ok, _ := l.Allow(context.Background(), "b", 1) // 其他 key 不被牵连阻塞
		done <- [2]bool{ok, false}
	}()

	for i := 0; i < 2; i++ {
		select {
		case r := <-done:
			if !r[0] {
				t.Fatalf("热路径请求应放行（a-热路径=%v）", r)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("批发在途期间请求被阻塞（第 %d 个），违反 N1 锁纪律", i+1)
		}
	}
	release()
}

func TestLeaderCtxCancelNotAffectFollowers(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	fb := &fakeBackend{fn: func(ctx context.Context, _ string, want int, _ Spec, _ GrantMode) (int, time.Duration, error) {
		entered <- struct{}{}
		<-gate
		return want, 0, nil
	}}
	clock := newFakeClock()
	l := New(fb, WithClock(clock), WithRate(100, time.Second), WithBurst(1000))
	defer l.Close()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	defer release()

	leaderCtx, cancel := context.WithCancel(context.Background())
	type res struct {
		ok  bool
		err error
	}
	leaderDone := make(chan res, 1)
	go func() {
		ok, err := l.Allow(leaderCtx, "k", 1)
		leaderDone <- res{ok, err}
	}()
	<-entered // leader 已登记 pending 并在批发中

	followerDone := make(chan res, 1)
	go func() {
		ok, err := l.Allow(context.Background(), "k", 1)
		followerDone <- res{ok, err}
	}()
	time.Sleep(20 * time.Millisecond) // 让 follower 入队共享

	cancel()  // 取消 leader 自己的 ctx——不得殃及批发与 followers
	release() // 放行批发（leader 用内部 ctx，结果照常结算）

	lr := <-leaderDone
	if !errors.Is(lr.err, context.Canceled) || lr.ok {
		t.Fatalf("leader 应透传自身 ctx.Canceled，got ok=%v err=%v", lr.ok, lr.err)
	}
	fr := <-followerDone
	if !fr.ok || fr.err != nil {
		t.Fatalf("leader ctx 取消不得殃及 follower，got ok=%v err=%v", fr.ok, fr.err)
	}
	if errors.Is(fr.err, ErrFailOpen) {
		t.Fatalf("follower 错误形态异常: %v", fr.err)
	}
}

func TestSilencePeriod(t *testing.T) {
	clock := newFakeClock()
	fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		return 0, 5 * time.Second, nil // 拒绝并提示 5s 后重试
	}}
	l := New(fb, WithClock(clock), WithRate(10, time.Second), WithBurst(10))
	defer l.Close()

	// 第 1 次：触发批发，granted==0 → 进入静默期。
	ok, err := l.Allow(context.Background(), "k", 1)
	var xe *ExceededError
	if ok || !errors.As(err, &xe) || xe.RetryAfter <= 0 {
		t.Fatalf("首次应超限且带 RetryAfter，got ok=%v err=%v", ok, err)
	}
	if calls := fb.count(); calls != 1 {
		t.Fatalf("批发应调用 1 次，got %d", calls)
	}

	// 静默期内：直接超限，不再触发批发。
	for i := 0; i < 5; i++ {
		if ok, err := l.Allow(context.Background(), "k", 1); ok || !errors.Is(err, ErrExceeded) {
			t.Fatalf("静默期内应直接超限，got ok=%v err=%v", ok, err)
		}
	}
	if calls := fb.count(); calls != 1 {
		t.Fatalf("静默期内不得重复批发，got %d 次", calls)
	}

	// 静默期过：允许再次批发。
	clock.Advance(6 * time.Second)
	if _, err := l.Allow(context.Background(), "k", 1); !errors.Is(err, ErrExceeded) {
		t.Fatalf("静默期后应重新批发仍超限，err=%v", err)
	}
	if calls := fb.count(); calls != 2 {
		t.Fatalf("静默期后批发应累计 2 次，got %d", calls)
	}
}

// --- 错误契约分诊表全覆盖 ---

func TestTriageExceeded(t *testing.T) {
	clock := newFakeClock()
	fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		return 0, 250 * time.Millisecond, nil
	}}
	l := New(fb, WithClock(clock), WithRate(10, time.Second), WithBurst(10))
	defer l.Close()

	ok, err := l.Allow(context.Background(), "key1", 3)
	if ok {
		t.Fatal("被拒请求 ok 应为 false")
	}
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("应满足 errors.Is(err, ErrExceeded)，got %v", err)
	}
	var xe *ExceededError
	if !errors.As(err, &xe) {
		t.Fatalf("应可 errors.As 到 *ExceededError，got %T", err)
	}
	if xe.Key != "key1" || xe.N != 3 || xe.RetryAfter != 250*time.Millisecond {
		t.Fatalf("ExceededError 字段异常: %+v", xe)
	}
}

func TestTriageCtxCancelPassthroughNoFailPolicy(t *testing.T) {
	unavailable := func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		return 0, 0, fmt.Errorf("%w: down", ErrBackendUnavailable)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// 租约与精确模式都要透传 ctx.Err()，绝不进 FailPolicy（H4）。
	for _, opts := range [][]Option{nil, {WithoutLocalLease()}} {
		opts = append(opts, WithFailPolicy(FailOpen))
		fb := &fakeBackend{fn: unavailable}
		l := New(fb, opts...)
		ok, err := l.Allow(ctx, "k", 1)
		if ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("ctx 取消应透传 ctx.Err()，got ok=%v err=%v", ok, err)
		}
		if errors.Is(err, ErrFailOpen) || errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("ctx 取消不得进 FailPolicy 分诊，got %v", err)
		}
		if calls := fb.count(); calls != 0 {
			t.Fatalf("ctx 取消应短路于后端之前，got %d 次调用", calls)
		}
		l.Close()
	}
}

func TestTriageUnavailableFailOpen(t *testing.T) {
	fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		return 0, 0, fmt.Errorf("%w: dial refused", ErrBackendUnavailable)
	}}
	l := New(fb, WithFailPolicy(FailOpen), WithRate(10, time.Second), WithBurst(10))
	defer l.Close()

	ok, err := l.Allow(context.Background(), "k", 1)
	if !ok {
		t.Fatalf("FailOpen 兜底应放行，got ok=%v", ok)
	}
	if !errors.Is(err, ErrFailOpen) || !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("兜底放行错误应双可判，got %v", err)
	}
	if err == nil || !contains(err.Error(), "dial refused") {
		t.Fatalf("兜底错误必须保留后端原文，got %v", err)
	}
}

func TestTriageUnavailableFailClosed(t *testing.T) {
	fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		return 0, 0, fmt.Errorf("%w: dial refused", ErrBackendUnavailable)
	}}
	l := New(fb, WithFailPolicy(FailClosed), WithRate(10, time.Second), WithBurst(10))
	defer l.Close()

	ok, err := l.Allow(context.Background(), "k", 1)
	if ok {
		t.Fatal("FailClosed 不得放行")
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("应可判 ErrBackendUnavailable，got %v", err)
	}
	if errors.Is(err, ErrFailOpen) {
		t.Fatalf("FailClosed 不得携带 ErrFailOpen，got %v", err)
	}
}

func TestTriageCommandErrorNoFallback(t *testing.T) {
	cmdErr := errors.New("WRONGTYPE key is not a zset")
	fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		return 0, 0, cmdErr
	}}
	// 即使 FailOpen，命令级错误也必须原样透传（防配置错误被兜底掩盖）。
	l := New(fb, WithFailPolicy(FailOpen), WithRate(10, time.Second), WithBurst(10))
	defer l.Close()

	ok, err := l.Allow(context.Background(), "k", 1)
	if ok {
		t.Fatal("命令级错误不得被 FailOpen 兜底放行")
	}
	if !errors.Is(err, cmdErr) {
		t.Fatalf("命令级错误应原样透传，got %v", err)
	}
	if errors.Is(err, ErrFailOpen) || errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("命令级错误不得挂兜底哨兵，got %v", err)
	}
}

func TestTriageAfterClose(t *testing.T) {
	fb := &fakeBackend{fn: grantAll}
	l := New(fb, WithRate(10, time.Second), WithBurst(10))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	ok, err := l.Allow(context.Background(), "k", 1)
	if ok || !errors.Is(err, ErrClosed) {
		t.Fatalf("Close 后 Allow 应返回 ErrClosed，got ok=%v err=%v", ok, err)
	}
	if err := l.Wait(context.Background(), "k", 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Close 后 Wait 应返回 ErrClosed，got %v", err)
	}
	if calls := fb.count(); calls != 0 {
		t.Fatalf("Close 后不得触后端，got %d 次", calls)
	}
}

// --- 精确模式（AllOrNothing 防蒸发，H3）---

func TestAllOrNothingNoEvaporation(t *testing.T) {
	clock := newFakeClock()
	mem := newMemoryBackend(clock)
	l := New(mem, WithoutLocalLease(), WithClock(clock), WithRate(10, time.Second), WithBurst(10))
	defer l.Close()

	ctx := context.Background()
	if ok, err := l.Allow(ctx, "k", 4); !ok || err != nil {
		t.Fatalf("满桶扣 4 应放行: %v %v", ok, err)
	}
	if ok, err := l.Allow(ctx, "k", 4); !ok || err != nil {
		t.Fatalf("剩 6 扣 4 应放行: %v %v", ok, err)
	}
	// 剩 2，不足额请求 7：必须拒绝且**不扣减**（防蒸发）。
	ok, err := l.Allow(ctx, "k", 7)
	if ok || !errors.Is(err, ErrExceeded) {
		t.Fatalf("不足额应拒绝，got ok=%v err=%v", ok, err)
	}
	// 拒绝未扣任何令牌：存量必须仍是 2。
	mem.mu.Lock()
	tokens := mem.buckets["k"].tokens
	mem.mu.Unlock()
	if tokens != 2 {
		t.Fatalf("AllOrNothing 拒绝后存量应仍为 2，got %v", tokens)
	}
	// 总放行量 == 桶理论量 10：4+4 已消耗 + 剩余 2。
	if ok, _ := l.Allow(ctx, "k", 2); !ok {
		t.Fatal("剩余 2 应可放行（证明拒绝未蒸发）")
	}
	if ok, _ := l.Allow(ctx, "k", 1); ok {
		t.Fatal("桶已耗尽，不应再放行")
	}
}

func TestStrictModeUsesAllOrNothingMode(t *testing.T) {
	fb := &fakeBackend{fn: grantAll}
	l := New(fb, WithoutLocalLease(), WithRate(10, time.Second), WithBurst(10))
	defer l.Close()

	if _, err := l.Allow(context.Background(), "k", 2); err != nil {
		t.Fatal(err)
	}
	if ok, err := l.Allow(context.Background(), "k", 2); !ok || err != nil {
		t.Fatalf("grantAll 下应放行: %v %v", ok, err)
	}
	for _, m := range fb.modes() {
		if m != GrantAllOrNothing {
			t.Fatalf("精确模式批发必须用 GrantAllOrNothing，got %v", m)
		}
	}
}

// --- 参数契约（M4）---

func TestParamContract(t *testing.T) {
	fb := &fakeBackend{fn: grantAll}
	l := New(fb, WithRate(10, time.Second), WithBurst(5))
	defer l.Close()

	cases := []struct {
		name string
		key  string
		n    int
	}{
		{"空 key", "", 1},
		{"n 为零", "k", 0},
		{"n 为负", "k", -3},
		{"n 超过 Burst", "k", 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := l.Allow(context.Background(), tc.key, tc.n)
			if ok || !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("应返回 ErrInvalidArgument，got ok=%v err=%v", ok, err)
			}
			if err := l.Wait(context.Background(), tc.key, tc.n); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("Wait 应同规则，got %v", err)
			}
		})
	}
	if calls := fb.count(); calls != 0 {
		t.Fatalf("参数错误不得触后端，got %d 次调用", calls)
	}
}

func TestSingleAllowAtMostOneWholesale(t *testing.T) {
	fb := &fakeBackend{fn: grantAll}
	l := New(fb, WithRate(100, time.Second), WithBurst(1000))
	defer l.Close()

	// n=200 > want=50：批发一次注入后仍不足，必须直接超限返回而非
	// 循环补批发（至多一次）。
	ok, err := l.Allow(context.Background(), "k", 200)
	if ok || !errors.Is(err, ErrExceeded) {
		t.Fatalf("仍不足时应返回超限，got ok=%v err=%v", ok, err)
	}
	if calls := fb.count(); calls != 1 {
		t.Fatalf("单次 Allow 至多一次批发，got %d 次", calls)
	}
}

// --- Wait 四出口 ---

func TestWaitSuccess(t *testing.T) {
	clock := newFakeClock()
	l := New(newMemoryBackend(clock), WithClock(clock), WithRate(100, time.Second), WithBurst(100))
	defer l.Close()

	if err := l.Wait(context.Background(), "k", 10); err != nil {
		t.Fatalf("存量充足 Wait 应立即成功，got %v", err)
	}
}

func TestWaitCtxCancel(t *testing.T) {
	clock := newFakeClock()
	fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		return 0, time.Hour, nil // 永远超限，令 Wait 进入等待
	}}
	l := New(fb, WithClock(clock), WithRate(10, time.Second), WithBurst(10))
	l.after = clock.after // 等待走可控时钟（不真睡）
	defer l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Wait(ctx, "k", 1) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 取消应返回 ctx.Err()，got %v", err)
	}
}

func TestWaitMaxWaitExceeded(t *testing.T) {
	clock := newFakeClock()
	fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		return 0, time.Hour, nil
	}}
	l := New(fb, WithClock(clock), WithRate(10, time.Second), WithBurst(10), WithMaxWait(time.Hour))
	l.after = clock.after
	defer l.Close()

	// 后台按可控时钟推进：每 1ms 推 1 分钟，模拟时间流逝直至 maxWait。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				clock.Advance(time.Minute)
			}
		}
	}()

	err := l.Wait(context.Background(), "k", 1)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("超出 WithMaxWait 应返回 ErrExceeded 语义错误，got %v", err)
	}
	var xe *ExceededError
	if !errors.As(err, &xe) {
		t.Fatalf("maxWait 错误应为 *ExceededError，got %T", err)
	}
}

func TestWaitBackendUnavailableStops(t *testing.T) {
	for _, policy := range []FailPolicy{FailOpen, FailClosed} {
		fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
			return 0, 0, fmt.Errorf("%w: down", ErrBackendUnavailable)
		}}
		l := New(fb, WithFailPolicy(policy), WithRate(10, time.Second), WithBurst(10))
		err := l.Wait(context.Background(), "k", 1)
		if err == nil {
			t.Fatalf("policy=%v 后端不可用必须返回错误（防吞错死循环）", policy)
		}
		if errors.Is(err, ErrExceeded) {
			t.Fatalf("后端不可用错误不得伪装成超限，got %v", err)
		}
		if !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("应可判 ErrBackendUnavailable，got %v", err)
		}
		if isOpen := policy == FailOpen; errors.Is(err, ErrFailOpen) != isOpen {
			t.Fatalf("policy=%v ErrFailOpen 可判性错误，got %v", policy, err)
		}
		if calls := fb.count(); calls != 1 {
			t.Fatalf("后端不可用须立即终止循环，got %d 次调用", calls)
		}
		l.Close()
	}
}

// --- Close ---

func TestCloseIdempotentAndBackendCloser(t *testing.T) {
	var closes atomic.Int32
	fb := &fakeBackend{fn: grantAll}
	l := New(closerBackend{fakeBackend: fb, closes: &closes}, WithRate(10, time.Second), WithBurst(10))

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("幂等 Close 不应报错: %v", err)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("Backend io.Closer 应恰好调用 1 次，got %d", got)
	}
	if _, err := l.Allow(context.Background(), "k", 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Close 后 Allow 应 ErrClosed，got %v", err)
	}
}

// closerBackend 附加 io.Closer 能力。
type closerBackend struct {
	*fakeBackend
	closes *atomic.Int32
}

func (c closerBackend) Close() error {
	c.closes.Add(1)
	return nil
}

// --- Execute[T] ---

func TestExecutePassThrough(t *testing.T) {
	clock := newFakeClock()
	l := New(newMemoryBackend(clock), WithClock(clock), WithRate(100, time.Second), WithBurst(5))
	defer l.Close()

	v, err := Execute(context.Background(), l, "k", func(context.Context) (int, error) { return 42, nil })
	if v != 42 || err != nil {
		t.Fatalf("放行时应透传结果，got %v/%v", v, err)
	}
}

func TestExecuteBusinessError(t *testing.T) {
	clock := newFakeClock()
	l := New(newMemoryBackend(clock), WithClock(clock), WithRate(100, time.Second), WithBurst(5))
	defer l.Close()

	myErr := errors.New("boom")
	v, err := Execute(context.Background(), l, "k", func(context.Context) (int, error) { return 7, myErr })
	if v != 7 || !errors.Is(err, myErr) {
		t.Fatalf("fn 错误应原样透传，got %v/%v", v, err)
	}
}

func TestExecuteExceededSkipsFn(t *testing.T) {
	clock := newFakeClock()
	l := New(newMemoryBackend(clock), WithClock(clock), WithRate(1, time.Second), WithBurst(1))
	defer l.Close()

	var fnCalled bool
	if _, err := Execute(context.Background(), l, "k", func(context.Context) (int, error) {
		fnCalled = true
		return 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	// 桶已耗尽：fn 不得执行，返回零值 + 超限错误。
	v, err := Execute(context.Background(), l, "k", func(context.Context) (int, error) {
		fnCalled = false
		panic("不应执行")
	})
	if v != 0 || !errors.Is(err, ErrExceeded) {
		t.Fatalf("超限应短路，got %v/%v", v, err)
	}
	if !fnCalled {
		t.Fatal("首次 Execute 应执行 fn")
	}
}

func TestExecuteFailOpenRunsFnButPerceivable(t *testing.T) {
	fb := &fakeBackend{fn: func(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
		return 0, 0, fmt.Errorf("%w: down", ErrBackendUnavailable)
	}}
	l := New(fb, WithFailPolicy(FailOpen), WithRate(10, time.Second), WithBurst(10))
	defer l.Close()

	v, err := Execute(context.Background(), l, "k", func(context.Context) (string, error) { return "ok", nil })
	if v != "ok" {
		t.Fatalf("FailOpen 下 fn 应执行且结果透传，got %v", v)
	}
	if !errors.Is(err, ErrFailOpen) {
		t.Fatalf("兜底放行应可感知，got %v", err)
	}
}

// --- FailOpen 兜底日志纪律（N-F）：同一批发结果仅 leader 记一条、锁外 ---

// capturingHandler 是极简 slog.Handler：计数 Warn 及以上级别记录。
type capturingHandler struct {
	mu      sync.Mutex
	warns   int
	records []string
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Level >= slog.LevelWarn {
		h.warns++
		h.records = append(h.records, r.Message)
	}
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.warns
}

func unavailableFn(context.Context, string, int, Spec, GrantMode) (int, time.Duration, error) {
	return 0, 0, fmt.Errorf("%w: dial refused", ErrBackendUnavailable)
}

func TestFailOpenLogsOncePerMergedBatch(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()

	var calls atomic.Int32
	h := &capturingHandler{}
	fb := &fakeBackend{fn: func(ctx context.Context, key string, want int, spec Spec, mode GrantMode) (int, time.Duration, error) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-gate // leader 批发在途，等 followers 全部挂上共享结果
		}
		return unavailableFn(ctx, key, want, spec, mode)
	}}
	clock := newFakeClock()
	l := New(fb, WithClock(clock), WithLogger(slog.New(h)),
		WithRate(100, time.Second), WithBurst(1000), WithFailPolicy(FailOpen))
	defer l.Close()

	type res struct {
		ok  bool
		err error
	}
	dones := make(chan res, 3)
	for i := 0; i < 3; i++ {
		go func() {
			ok, err := l.Allow(context.Background(), "k", 1)
			dones <- res{ok, err}
		}()
		if i == 0 {
			<-entered // 确保 leader 已登记 pending 并在批发中
		}
	}
	time.Sleep(20 * time.Millisecond) // 让两个 follower 挂上共享等待
	release()                         // leader 随后拿到不可用错误并广播

	for i := 0; i < 3; i++ {
		r := <-dones
		if !r.ok || !errors.Is(r.err, ErrFailOpen) {
			t.Fatalf("三请求均应 FailOpen 兜底放行且错误可判，got ok=%v err=%v", r.ok, r.err)
		}
	}
	// 合并批次的裁决由 leader 批发产生：日志恰 1 条（followers 不重复）。
	if n := h.count(); n != 1 {
		t.Fatalf("同一批发结果的 FailOpen 日志应恰 1 条，got %d", n)
	}
}

func TestFailOpenLogsOncePerStrictWholesale(t *testing.T) {
	h := &capturingHandler{}
	fb := &fakeBackend{fn: unavailableFn}
	l := New(fb, WithLogger(slog.New(h)), WithoutLocalLease(),
		WithRate(10, time.Second), WithBurst(10), WithFailPolicy(FailOpen))
	defer l.Close()

	for i := 0; i < 3; i++ {
		ok, err := l.Allow(context.Background(), "k", 1)
		if !ok || !errors.Is(err, ErrFailOpen) {
			t.Fatalf("strict FailOpen 应放行可判: %v %v", ok, err)
		}
	}
	// 精确模式每次请求 = 一次独立批发：每次各记一条（共 3 条）。
	if n := h.count(); n != 3 {
		t.Fatalf("strict 模式每次独立批发应各记 1 条日志，got %d", n)
	}
	// FailClosed 不记兜底日志（无兜底放行事件）。
	h2 := &capturingHandler{}
	fb2 := &fakeBackend{fn: unavailableFn}
	l2 := New(fb2, WithLogger(slog.New(h2)), WithRate(10, time.Second), WithBurst(10), WithFailPolicy(FailClosed))
	defer l2.Close()
	if _, err := l2.Allow(context.Background(), "k", 1); errors.Is(err, ErrFailOpen) {
		t.Fatal("FailClosed 不得放行")
	}
	if n := h2.count(); n != 0 {
		t.Fatalf("FailClosed 无兜底放行事件，不应有日志，got %d", n)
	}
}

// contains 是 strings.Contains 的最小替代（避免测试文件多余 import 噪音）。
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
