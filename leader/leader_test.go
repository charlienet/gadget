package leader

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charlienet/gadget/lock"
)

// 测试参数基线（ms 级真实定时器，对齐 lock_test.go 风格；
// 设计稿 §6.2 参数：Lease=200ms / Deadline=120ms / Retry=20ms）。
const (
	tLease    = 200 * time.Millisecond
	tDeadline = 120 * time.Millisecond
	tRetry    = 20 * time.Millisecond
)

// newTestElector 构造 ms 级参数的 Elector（测试专用快捷方式）。
func newTestElector(f *fakeLocker, cb Callbacks) *Elector {
	return New(WithLocker(f), WithCallbacks(cb),
		WithLeaseDuration(tLease), WithRenewDeadline(tDeadline), WithRetryPeriod(tRetry))
}

// waitForCount 轮询 fake 的某类事件数直至达到 n 次或超时。
func waitForCount(t *testing.T, f *fakeLocker, ev string, n int) {
	t.Helper()
	if !waitFor(func() bool { return f.eventCount(ev) >= n }, 3*time.Second) {
		t.Fatalf("等待事件 %q ×%d 超时（现有日志 %v）", ev, n, f.eventSeq())
	}
}

// waitForCalls 轮询 atomic 计数直至达到 n 或超时。
func waitForCalls(t *testing.T, c *atomic.Int64, n int64) {
	t.Helper()
	if !waitFor(func() bool { return c.Load() >= n }, 3*time.Second) {
		t.Fatalf("等待调用数 ≥%d 超时（当前 %d）", n, c.Load())
	}
}

// waitFor 轮询 cond 直到成立或超时，返回是否成立。
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.After(timeout)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return true
		}
		select {
		case <-deadline:
			return false
		case <-tick.C:
		}
	}
}

// filterEvents 过滤事件日志（剔除 tryLock/renew 噪音，保留回调骨架断言）。
func filterEvents(events []string, want ...string) []string {
	set := map[string]bool{}
	for _, w := range want {
		set[w] = true
	}
	var out []string
	for _, ev := range events {
		if set[ev] {
			out = append(out, ev)
		}
	}
	return out
}

func assertEventSeq(t *testing.T, f *fakeLocker, want ...string) {
	t.Helper()
	got := filterEvents(f.eventSeq(), want...)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列 = %v, want %v（全量日志 %v）", got, want, f.eventSeq())
	}
}

func noCallbacks(t *testing.T, f *fakeLocker) {
	t.Helper()
	if n := f.eventCount("started") + f.eventCount("stopped"); n != 0 {
		t.Fatalf("不应有任何回调，events=%v", f.eventSeq())
	}
}

func startedBlockUntilDone(f *fakeLocker) func(context.Context, uint64) {
	// 典型长驻业务：记录 started 后阻塞至业务 ctx 被让位流程取消。
	return func(ctx context.Context, _ uint64) {
		f.record("started")
		<-ctx.Done()
	}
}

func runAsync(e *Elector, ctx context.Context) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- e.Run(ctx) }()
	return ch
}

// --- 构造校验（T1-T4）---

func TestNew_PanicMissingLocker(t *testing.T) { // T1
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("缺 WithLocker 应 panic")
		}
	}()
	_ = New(WithCallbacks(Callbacks{OnStartedLeading: func(context.Context, uint64) {}}))
}

func TestNew_PanicMissingOnStartedLeading(t *testing.T) { // T2
	cases := []struct {
		name string
		opts []Option
	}{
		{"Callbacks 零值", []Option{WithLocker(&fakeLocker{})}},
		{"显式 nil started", []Option{WithLocker(&fakeLocker{}), WithCallbacks(Callbacks{})}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("OnStartedLeading 为 nil 应 panic")
				}
			}()
			_ = New(c.opts...)
		})
	}
}

func TestNew_PanicInvalidTiming(t *testing.T) { // T3
	cases := []struct {
		name                   string
		lease, deadline, retry time.Duration
	}{
		{"Lease==Deadline", 100 * time.Millisecond, 100 * time.Millisecond, 20 * time.Millisecond},
		{"Lease<Deadline", 90 * time.Millisecond, 100 * time.Millisecond, 20 * time.Millisecond},
		{"Deadline==Retry", 200 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond},
		{"Deadline<Retry", 200 * time.Millisecond, 40 * time.Millisecond, 50 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("%v/%v/%v 应 panic", c.lease, c.deadline, c.retry)
				}
			}()
			_ = New(WithLocker(&fakeLocker{}),
				WithCallbacks(Callbacks{OnStartedLeading: func(context.Context, uint64) {}}),
				WithLeaseDuration(c.lease), WithRenewDeadline(c.deadline), WithRetryPeriod(c.retry))
		})
	}
}

func TestNew_Defaults(t *testing.T) { // T4
	e := New(WithLocker(&fakeLocker{}),
		WithCallbacks(Callbacks{OnStartedLeading: func(context.Context, uint64) {}}))
	if ok, err := regexp.MatchString(`^.+-\d+$`, e.Identity()); err != nil || !ok {
		t.Fatalf("Identity = %q, want hostname-pid 形态", e.Identity())
	}
	if e.Term() != 0 {
		t.Fatalf("Term = %d, want 0", e.Term())
	}
	if e.IsLeader() {
		t.Fatal("IsLeader = true, want false")
	}
	if e.leaseDuration != defaultLeaseDuration || e.renewDeadline != defaultRenewDeadline || e.retryPeriod != defaultRetryPeriod {
		t.Fatal("默认时长与设计稿不一致")
	}
}

func TestOptionsIgnoreInvalid(t *testing.T) {
	o := defaultOptions()
	WithLocker(nil)(o)
	WithIdentity("")(o)
	WithLeaseDuration(0)(o)
	WithLeaseDuration(-time.Second)(o)
	WithRenewDeadline(0)(o)
	WithRetryPeriod(0)(o)
	if o.locker != nil {
		t.Fatal("WithLocker(nil) 不应生效")
	}
	if o.identity == "" || o.leaseDuration != defaultLeaseDuration ||
		o.renewDeadline != defaultRenewDeadline || o.retryPeriod != defaultRetryPeriod {
		t.Fatalf("非法值应全部忽略，得到 %+v", o)
	}
	// 合法值生效
	WithIdentity("n1")(o)
	f := &fakeLocker{}
	WithLocker(f)(o)
	if o.identity != "n1" || o.locker != Locker(f) {
		t.Fatal("合法值应生效")
	}
}

// --- 竞选阶段（T5/T6/T12）---

func TestRun_AcquireCancelled(t *testing.T) { // T5
	f := &fakeLocker{} // tryLockResult 默认 false：恒被他人持有
	e := newTestElector(f, Callbacks{OnStartedLeading: startedBlockUntilDone(f)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := e.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	noCallbacks(t, f)
	if f.tryLockCalls.Load() < 2 {
		t.Fatalf("tryLockCalls = %d, want ≥2（确实在轮询）", f.tryLockCalls.Load())
	}
	if e.IsLeader() {
		t.Fatal("IsLeader 应恒为 false")
	}
	if f.unlockCalls.Load() != 0 {
		t.Fatalf("从未获锁不应 Unlock（calls=%d）", f.unlockCalls.Load())
	}
}

func TestRun_AcquireErrorKeepsRetrying(t *testing.T) { // T6
	f := &fakeLocker{tryLockErr: fmt.Errorf("%w: connection refused", lock.ErrBackendUnavailable)}
	e := newTestElector(f, Callbacks{OnStartedLeading: startedBlockUntilDone(f)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := e.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("后端错误不得终止竞选: err = %v, want context.Canceled", err)
	}
	noCallbacks(t, f)
	if f.tryLockCalls.Load() < 2 {
		t.Fatalf("tryLockCalls = %d, want ≥2", f.tryLockCalls.Load())
	}
}

func TestRun_FailOpenGuard_TryLock(t *testing.T) { // T12
	// FailOpen 误配：(true, err) 组合，err 非 nil 一律不信任 ok。
	f := &fakeLocker{
		tryLockResult: true,
		tryLockErr:    fmt.Errorf("%w: failopen passthrough", lock.ErrBackendUnavailable),
	}
	e := newTestElector(f, Callbacks{OnStartedLeading: startedBlockUntilDone(f)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := e.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled（不得当选）", err)
	}
	noCallbacks(t, f)
	if f.unlockCalls.Load() != 0 {
		t.Fatal("(true,err) 组合不得被视为当选，不应触发清理")
	}
}

// --- 当选与让位路径（T7/T8/T14/T18）---

func TestRun_ElectedGracefulExit(t *testing.T) { // T7
	f := newFake() // renewOK 默认 true：任期健康
	f.tryLockSeq = []bool{false, true}
	resume := make(chan struct{})
	f.unlockWait = resume // 门控 unlock：业务观察事件落定后再放行，确定事件序
	var seenTerm atomic.Uint64
	e := newTestElector(f, Callbacks{
		OnStartedLeading: func(ctx context.Context, term uint64) {
			seenTerm.Store(term)
			f.record("started")
			<-ctx.Done()
			f.record("biz-observed-done")
		},
		OnStoppedLeading: func() { f.record("stopped") },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := runAsync(e, ctx)

	waitForCount(t, f, "started", 1)
	waitForCalls(t, &f.renewCalls, 1) // 确保 cancel 前已发生至少一次成功续约（TTL 断言前提）
	if !e.IsLeader() {
		t.Fatal("任期中 IsLeader 应为 true")
	}
	cancel()
	waitForCount(t, f, "biz-observed-done", 1) // stepDown 已 cancelLead，业务已观察
	close(resume)                              // 放行 unlock → stopped
	err := <-runDone
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if seenTerm.Load() != 1 {
		t.Fatalf("started 收到 term = %d, want 1", seenTerm.Load())
	}
	assertEventSeq(t, f, "started", "biz-observed-done", "unlock", "stopped")
	if f.eventCount("stopped") != 1 {
		t.Fatalf("stopped 应恰好 1 次")
	}
	if f.unlockCalls.Load() != 1 {
		t.Fatalf("unlockCalls = %d, want 1", f.unlockCalls.Load())
	}
	if e.Term() != 1 {
		t.Fatalf("Term = %d, want 1", e.Term())
	}
	if e.IsLeader() {
		t.Fatal("让位后 IsLeader 应为 false")
	}
	if got := f.renewLastTTL.Load(); got != int64(tLease) {
		t.Fatalf("renewLastTTL = %v, want %v", time.Duration(got), tLease)
	}
}

func TestRun_CancelledAtAcquireMoment(t *testing.T) { // T8
	// TryLock 返回 (true, nil) 但返回时刻 ctx 已取消：当选确认第一步
	// 检出，零回调、零 term 消耗、仍尽力释放锁。hold+holdResult 钩子
	// 使"取消先于 TryLock 返回"完全确定（fake 无视取消照常成功）。
	f := &fakeLocker{tryLockResult: true, tryLockHold: make(chan struct{}), tryLockHoldResult: true}
	e := newTestElector(f, Callbacks{OnStartedLeading: startedBlockUntilDone(f)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := runAsync(e, ctx)

	waitForCalls(t, &f.tryLockCalls, 1)
	time.Sleep(20 * time.Millisecond) // 让 A goroutine 确定停在 hold 上
	cancel()                          // 先取消……
	close(f.tryLockHold)              // ……再放行 TryLock 返回 (true, nil)
	err := <-runDone
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	noCallbacks(t, f)
	if e.Term() != 0 {
		t.Fatalf("Term = %d, want 0（R1：当选瞬间取消零 term 消耗）", e.Term())
	}
	if f.unlockCalls.Load() != 1 {
		t.Fatalf("unlockCalls = %d, want 1（当选瞬间取消仍尽力释放）", f.unlockCalls.Load())
	}
	if e.IsLeader() {
		t.Fatal("IsLeader 应为 false")
	}
}

func TestRun_ResignOnStartedReturn(t *testing.T) { // T14
	f := newFake()
	f.tryLockResult = true // 竞选即当选（newFake 的 TryLock 默认失败）
	e := newTestElector(f, Callbacks{
		OnStartedLeading: func(context.Context, uint64) { f.record("started") }, // 立即返回=主动让位
		OnStoppedLeading: func() { f.record("stopped") },
	})
	err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("主动让位应返回 nil，got %v", err)
	}
	assertEventSeq(t, f, "started", "unlock", "stopped")
	if f.unlockCalls.Load() != 1 {
		t.Fatalf("unlockCalls = %d, want 1", f.unlockCalls.Load())
	}
	if e.Term() != 1 {
		t.Fatalf("Term = %d, want 1", e.Term())
	}
}

func TestRun_NilOnStoppedLeadingSkipped(t *testing.T) { // S8
	f := newFake()
	f.tryLockResult = true // 竞选即当选（newFake 的 TryLock 默认失败）
	e := newTestElector(f, Callbacks{
		OnStartedLeading: func(context.Context, uint64) { f.record("started") },
		// OnStoppedLeading 为 nil：跳过，其余行为不变
	})
	err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	assertEventSeq(t, f, "started", "unlock")
	if f.eventCount("stopped") != 0 {
		t.Fatal("nil 回调不应产生 stopped 事件")
	}
	if f.unlockCalls.Load() != 1 {
		t.Fatalf("unlockCalls = %d, want 1（清理仍完整）", f.unlockCalls.Load())
	}
}

func TestRun_StartedLeadingDoesNotBlockRenew(t *testing.T) { // T18
	f := newFake()
	f.tryLockResult = true // 竞选即当选（newFake 的 TryLock 默认失败）
	bizExit := make(chan struct{})
	e := newTestElector(f, Callbacks{
		OnStartedLeading: func(_ context.Context, _ uint64) {
			f.record("started")
			<-bizExit // 业务顽固不退出（连 leadCtx 都不看）
		},
		OnStoppedLeading: func() { f.record("stopped") },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := runAsync(e, ctx)
	waitForCount(t, f, "started", 1)

	cancel()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 被业务回调阻塞（违反不变量：不等待 OnStartedLeading）")
	}
	// Run 已返回而业务 goroutine 仍在运行 → 证明 stepDown 未等待它。
	assertEventSeq(t, f, "started", "unlock", "stopped")
	close(bizExit)
}

// --- 续约路径（T9/T10/T11/T13/T15）---

func TestRun_LostOnRenewFalse(t *testing.T) { // T9
	f := &fakeLocker{tryLockResult: true, renewOK: false} // (false, nil)=确认丢失
	e := newTestElector(f, Callbacks{
		OnStartedLeading: startedBlockUntilDone(f),
		OnStoppedLeading: func() { f.record("stopped") },
	})
	start := time.Now()
	err := e.Run(context.Background())
	elapsed := time.Since(start)
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("err = %v, want ErrLeadershipLost", err)
	}
	if elapsed >= tDeadline {
		t.Fatalf("耗时 %v, want < %v（确定性丢失立即让位，不等预算）", elapsed, tDeadline)
	}
	if f.eventCount("stopped") != 1 {
		t.Fatalf("stopped 应恰好 1 次")
	}
	if f.unlockCalls.Load() != 1 {
		t.Fatalf("unlockCalls = %d, want 1", f.unlockCalls.Load())
	}
	if !strings.Contains(err.Error(), "renew confirmed lock lost") {
		t.Fatalf("err 消息应含固定文案（S2），got %q", err.Error())
	}
}

func TestRun_LostOnRenewDeadline(t *testing.T) { // T10（阻塞续约变体：deadline 独立兜底）
	// renewBlock 恒阻塞：在途续约被 renewCtx 预算超时打断，
	// deadlineTimer 同时到点——两条路径收敛于预算耗尽让位，耗时精确
	// ≈Deadline（S3：多信号就绪时 select 随机，均为合法分类）。
	f := &fakeLocker{tryLockResult: true, renewBlock: make(chan struct{})}
	e := newTestElector(f, Callbacks{
		OnStartedLeading: startedBlockUntilDone(f),
		OnStoppedLeading: func() { f.record("stopped") },
	})
	start := time.Now()
	err := e.Run(context.Background())
	elapsed := time.Since(start)
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("err = %v, want ErrLeadershipLost", err)
	}
	if elapsed < tDeadline || elapsed > tDeadline+2*tRetry {
		t.Fatalf("耗时 %v, want ∈ [%v, %v]", elapsed, tDeadline, tDeadline+2*tRetry)
	}
	if got := f.renewCalls.Load(); got < 1 {
		t.Fatalf("renewCalls = %d, want ≥1", got)
	}
	if !strings.Contains(err.Error(), "deadline exceeded") && !strings.Contains(err.Error(), "renew confirmed lock lost") {
		t.Fatalf("err 消息应含预算超时/确认丢失文案，got %q", err.Error())
	}
	if f.eventCount("stopped") != 1 {
		t.Fatal("stopped 应恰好 1 次")
	}
}

func TestRun_LostOnRenewPersistentError(t *testing.T) { // T10 主体：恒 err → 预算耗尽
	f := &fakeLocker{tryLockResult: true, renewErr: errors.New("renew boom")}
	e := newTestElector(f, Callbacks{
		OnStartedLeading: startedBlockUntilDone(f),
		OnStoppedLeading: func() { f.record("stopped") },
	})
	start := time.Now()
	err := e.Run(context.Background())
	elapsed := time.Since(start)
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("err = %v, want ErrLeadershipLost", err)
	}
	if elapsed < tDeadline || elapsed > tDeadline+3*tRetry {
		t.Fatalf("耗时 %v, want ∈ [%v, %v]", elapsed, tDeadline, tDeadline+3*tRetry)
	}
	if got := f.renewCalls.Load(); got < 4 { // R3
		t.Fatalf("renewCalls = %d, want ≥4", got)
	}
	if !strings.Contains(err.Error(), "renew boom") {
		t.Fatalf("err 消息应含最后一次续约错误文本，got %q", err.Error())
	}
}

func TestRun_RenewFlapThenRecover(t *testing.T) { // T11（S4：前 1 次失败，恢复点余量 ≥3×）
	f := newFake()
	f.tryLockResult = true
	f.renewOKAfterN = 1
	f.renewErr = errors.New("transient")
	e := newTestElector(f, Callbacks{
		OnStartedLeading: startedBlockUntilDone(f),
		OnStoppedLeading: func() { f.record("stopped") },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := runAsync(e, ctx)

	waitForCount(t, f, "started", 1)
	waitForCalls(t, &f.renewCalls, 2) // 第 1 次失败、第 2 次成功后 deadlineTimer 已重置
	// 观察窗 1.9×Deadline < 预算重置后的下一个 2×Deadline：Run 不应返回。
	select {
	case err := <-runDone:
		t.Fatalf("抖动自愈后 Run 提前返回: %v", err)
	case <-time.After(190 * time.Millisecond):
	}
	if !e.IsLeader() {
		t.Fatal("观察窗内 IsLeader 应恒为 true")
	}
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if f.eventCount("stopped") != 1 {
		t.Fatal("最终让位 stopped 恰 1 次")
	}
}

func TestRun_FailOpenGuard_Renew(t *testing.T) { // T13
	// (true, err) 组合按失败处理：预算耗尽让位，不因 ok=true 续任。
	f := &fakeLocker{
		tryLockResult: true,
		renewOK:       true,
		renewErr:      fmt.Errorf("%w: failopen renew", lock.ErrBackendUnavailable),
	}
	e := newTestElector(f, Callbacks{
		OnStartedLeading: startedBlockUntilDone(f),
		OnStoppedLeading: func() { f.record("stopped") },
	})
	err := e.Run(context.Background())
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("err = %v, want ErrLeadershipLost", err)
	}
	if f.renewCalls.Load() < 4 { // R3 数学同 T10
		t.Fatalf("renewCalls = %d, want ≥4", f.renewCalls.Load())
	}
}

func TestRun_RenewUnsupported(t *testing.T) { // T15
	f := &fakeLocker{tryLockResult: true, renewErr: lock.ErrRenewUnsupported}
	e := newTestElector(f, Callbacks{
		OnStartedLeading: startedBlockUntilDone(f),
		OnStoppedLeading: func() { f.record("stopped") },
	})
	start := time.Now()
	err := e.Run(context.Background())
	elapsed := time.Since(start)
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("err = %v, want 命中 ErrLeadershipLost", err)
	}
	if !errors.Is(err, lock.ErrRenewUnsupported) {
		t.Fatalf("err = %v, want 同时命中 lock.ErrRenewUnsupported", err)
	}
	if elapsed >= tDeadline {
		t.Fatalf("耗时 %v, want < %v（致命配置错立即让位）", elapsed, tDeadline)
	}
	if f.eventCount("stopped") != 1 {
		t.Fatal("stopped 恰 1 次")
	}
}

// --- term / 并发 / 防御分支（T16-T21、T19）---

func TestRun_TermIncrementsAcrossRuns(t *testing.T) { // T16
	f := newFake()
	f.tryLockResult = true // 竞选即当选（两轮共用；newFake 的 TryLock 默认失败）
	var terms []uint64
	var mu sync.Mutex
	e := newTestElector(f, Callbacks{
		OnStartedLeading: func(ctx context.Context, term uint64) {
			mu.Lock()
			terms = append(terms, term)
			mu.Unlock()
			f.record("started")
			if term > 1 {
				<-ctx.Done() // 第二轮：阻塞至外层 ctx 取消（优雅退出路径）
			}
			// 第一轮：立即返回 = resigned，run1 干净结束
		},
		OnStoppedLeading: func() { f.record("stopped") },
	})
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run1 err = %v, want nil", err)
	}
	// run2：竞选成功即当选，外层 ctx 取消走优雅退出（run2 干净结束）
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	runDone := runAsync(e, ctx2)
	waitForCount(t, f, "started", 2)
	cancel2()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("run2 err = %v, want context.Canceled", err)
	}
	mu.Lock()
	got := fmt.Sprint(terms)
	mu.Unlock()
	if got != "[1 2]" {
		t.Fatalf("started 收到的 term 序列 = %s, want [1 2]", got)
	}
	if e.Term() != 2 {
		t.Fatalf("Term 终态 = %d, want 2", e.Term())
	}
	if f.eventCount("stopped") != 2 {
		t.Fatalf("stopped 应成对出现 2 次，got %d", f.eventCount("stopped"))
	}
}

func TestRun_ConcurrentRunPanics(t *testing.T) { // T17
	f := &fakeLocker{tryLockResult: false, tryLockBlock: make(chan struct{})}
	e := newTestElector(f, Callbacks{OnStartedLeading: startedBlockUntilDone(f)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	aDone := runAsync(e, ctx)
	waitForCalls(t, &f.tryLockCalls, 1) // A 已进入 Run 并阻塞在 TryLock

	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_ = e.Run(ctx) // 第二次并发 Run
		panicked <- nil
	}()
	select {
	case r := <-panicked:
		if r == nil {
			t.Fatal("并发 Run 应 panic")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "并发 Run") {
			t.Fatalf("panic 消息 = %v, want 含「不支持并发 Run」", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("第二次 Run 未立即 panic")
	}
	close(f.tryLockBlock)
	cancel() // A 的 TryLock 放行后回到轮询循环，须以取消终止 A
	if err := <-aDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("A 应正常收尾返回 context.Canceled, got %v", err)
	}
}

func TestNextRetry_Bounds(t *testing.T) { // T19
	d := time.Duration(1_000_000) // 1ms
	for i := 0; i < 1000; i++ {
		got := nextRetry(d)
		if got < d || got >= d+d/4 {
			t.Fatalf("nextRetry(%v) = %v, want ∈ [%v, %v)", d, got, d, d+d/4)
		}
	}
	for _, tiny := range []time.Duration{1, 2, 3} {
		if got := nextRetry(tiny); got != tiny {
			t.Fatalf("nextRetry(%vns) = %v, want 原值（d/4 整除为 0 不 panic）", tiny, got)
		}
	}
}

func TestIsLeader_RaceSmoke(t *testing.T) { // T20
	f := &fakeLocker{tryLockResult: true}
	e := newTestElector(f, Callbacks{OnStartedLeading: startedBlockUntilDone(f)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := runAsync(e, ctx)
	waitForCount(t, f, "started", 1)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = e.IsLeader()
				_ = e.Term()
				_ = e.Identity()
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	cancel()
	wg.Wait()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRun_UnlockErrorStillCompletes(t *testing.T) { // T21（R5）
	f := newFake()
	f.tryLockResult = true
	f.unlockErr = errors.New("unlock boom")
	e := newTestElector(f, Callbacks{
		OnStartedLeading: func(context.Context, uint64) { f.record("started") },
		OnStoppedLeading: func() { f.record("stopped") },
	})
	err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Unlock 失败不得改变返回值: err = %v, want nil（与 T14 成功路径一致）", err)
	}
	assertEventSeq(t, f, "started", "unlock", "stopped") // 回调序列与成功路径完全一致
	if f.unlockCalls.Load() != 1 {
		t.Fatalf("unlockCalls = %d, want 1", f.unlockCalls.Load())
	}

	// 变体：第二轮 Run 在 TryLock 前 N 次 false（模拟等待上一轮锁 TTL 自然
	// 过期）后成功当选。
	f.tryLockSeq = []bool{false, false, false, true}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run2 err = %v, want nil", err)
	}
	if e.Term() != 2 {
		t.Fatalf("Term = %d, want 2（让位不计 term，当选才计）", e.Term())
	}
	if f.eventCount("started") != 2 {
		t.Fatalf("started 应 2 次，got %d", f.eventCount("started"))
	}
}
