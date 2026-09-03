package lifecycle

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// mustPanic 断言 fn 触发 panic。
func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for %s, got none", want)
		}
	}()
	fn()
}

// waitSigCh 确定性等待 Run 完成 signal.Notify 并把句柄回写到 m.sigCh。
//
// Run 在 m.mu 的同一临界区内先 signal.Notify 再赋值 m.sigCh，因此轮询到非 nil
// 即严格等价于“信号句柄已注册并可投递”——这是对结果的有界等待，不是时序运气。
// 读 m.sigCh 时同样持 m.mu，与 Run 的写形成互斥，race detector 下安全。
func waitSigCh(t *testing.T, m *Manager) chan os.Signal {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		ch := m.sigCh
		m.mu.Unlock()
		if ch != nil {
			return ch
		}
		if time.Now().After(deadline) {
			t.Fatal("Run 未在 5s 内注册信号句柄（m.sigCh 仍为 nil）")
		}
		runtime.Gosched()
	}
}

// readSig 读取一个信号，超时返回 nil（用于证明信号确已送达进程）。
func readSig(ch chan os.Signal, d time.Duration) os.Signal {
	select {
	case s := <-ch:
		return s
	case <-time.After(d):
		return nil
	}
}

// 测试1：关闭严格按注册逆序执行。
func TestShutdownReverseOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	m := New()
	for _, name := range []string{"a", "b", "c"} {
		name := name
		m.Register(name, Func(func(context.Context) error {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}))
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []string{"c", "b", "a"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("stop order = %v, want %v", order, want)
	}
}

// 测试2：单步不响应 ctx 且永久阻塞 -> stepTimeout 生效、后续步骤继续、聚合含 ErrTimeout。
func TestStepTimeout(t *testing.T) {
	var mu sync.Mutex
	called := map[string]bool{}
	rec := func(name string) Component {
		return Func(func(context.Context) error {
			mu.Lock()
			called[name] = true
			mu.Unlock()
			return nil
		})
	}
	m := New(WithStepTimeout(50 * time.Millisecond))
	m.Register("fast1", rec("fast1"))
	// blocker 不响应 ctx、永久阻塞，且自身永不返回（泄漏的 goroutine 无碍测试）。
	m.Register("blocker", Func(func(context.Context) error {
		mu.Lock()
		called["blocker"] = true
		mu.Unlock()
		select {}
	}))
	m.Register("fast2", rec("fast2"))

	err := m.Shutdown(context.Background())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("聚合错误应含 ErrTimeout，实得 %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// 逆序: fast2 -> blocker(超时) -> fast1，blocker 超时后 fast1 仍应被执行。
	if !called["fast2"] || !called["blocker"] || !called["fast1"] {
		t.Fatalf("后续步骤未继续执行: %v", called)
	}
}

// 测试3：总预算耗尽 -> 剩余组件跳过，errors.As 取到 *SkippedError 名单正确。
func TestBudgetExhaustedSkips(t *testing.T) {
	m := New(WithTotalTimeout(80*time.Millisecond), WithStepTimeout(time.Second))
	m.Register("c1", Func(func(context.Context) error { return nil }))
	m.Register("c2", Func(func(context.Context) error { return nil }))
	// 逆序最先关 c3：永久阻塞，耗尽整个总预算后其步骤超时，剩余 c2/c1 应被跳过。
	m.Register("c3", Func(func(context.Context) error { select {} }))

	err := m.Shutdown(context.Background())
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("应含 ErrBudgetExhausted，实得 %v", err)
	}
	var se *SkippedError
	if !errors.As(err, &se) {
		t.Fatalf("应能 errors.As 取到 *SkippedError，实得 %v", err)
	}
	got := strings.Join(se.Names, ",")
	if got != "c1,c2" {
		t.Fatalf("跳过名单 = %q, want %q", got, "c1,c2")
	}
}

// 测试4：panic 组件 -> errors.Is ErrPanicked，后续步骤照常执行。
func TestPanicRecovered(t *testing.T) {
	var mu sync.Mutex
	called := map[string]bool{}
	rec := func(name string) Component {
		return Func(func(context.Context) error {
			mu.Lock()
			called[name] = true
			mu.Unlock()
			return nil
		})
	}
	m := New(WithStepTimeout(time.Second))
	m.Register("a", rec("a"))
	m.Register("boom", Func(func(context.Context) error { panic("kaboom") }))
	m.Register("z", rec("z"))

	err := m.Shutdown(context.Background())
	if !errors.Is(err, ErrPanicked) {
		t.Fatalf("应含 ErrPanicked，实得 %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// 逆序: z -> boom(panic) -> a；panic 不应中断后续步骤。
	if !called["z"] || !called["a"] {
		t.Fatalf("panic 后后续步骤未执行: %v", called)
	}
}

// 测试5a：Run 的 ctx 触发与 Shutdown 并发 -> 各方返回同一错误、关闭只执行一次。
func TestConcurrentRunAndShutdown(t *testing.T) {
	sentinel := errors.New("stop-failed")
	var count atomic.Int64
	m := New()
	m.Register("c", Func(func(context.Context) error {
		count.Add(1)
		return sentinel
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 2)
	go func() { res <- m.Run(ctx) }()
	go func() { res <- m.Shutdown(context.Background()) }()

	time.Sleep(20 * time.Millisecond)
	cancel() // 额外制造第三个触发源与 Shutdown 竞态

	e1 := <-res
	e2 := <-res
	if e1 == nil || !errors.Is(e1, sentinel) {
		t.Fatalf("e1 = %v, want wrapped sentinel", e1)
	}
	if e1 != e2 {
		t.Fatalf("并发调用应返回同一聚合错误: %v vs %v", e1, e2)
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("关闭应只执行一次, Stop 计数 = %d", got)
	}
}

// 测试5b：OS 信号触发路径 -> 关闭只执行一次；关闭完成后的 Shutdown 返回同一错误。
//
// 这里用 SIGWINCH 而不是 SIGTERM：SIGWINCH 的默认动作是忽略，即使句柄注销失败
// 或多发一发信号，也不会杀死测试二进制（SIGURG 不可用，Go runtime 抢占也会用它，
// 会误触发）。等待 signal.Notify 就绪改用 waitSigCh 做确定性同步，不再靠 sleep。
func TestSignalTriggersShutdown(t *testing.T) {
	var count atomic.Int64
	m := New(WithSignals(syscall.SIGWINCH))
	m.Register("c", Func(func(context.Context) error {
		count.Add(1)
		return nil
	}))

	done := make(chan error, 1)
	go func() { done <- m.Run(context.Background()) }()
	sigCh := waitSigCh(t, m) // 句柄已注册：Run 确已把 sigCh 回写进 m.sigCh
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("kill: %v", err)
	}
	e1 := <-done
	if e1 != nil {
		t.Fatalf("e1 = %v, want nil", e1)
	}
	// 关闭已完成：Run 已把句柄回写 m.sigCh，start() 也已对它执行 signal.Stop，
	// 因此此处不存在残留监听；Shutdown 只等待并复用同一结果。
	if n := len(sigCh); n != 0 {
		t.Fatalf("关闭结束后信号句柄仍有 %d 个未读信号", n)
	}
	e2 := m.Shutdown(context.Background())
	if e1 != e2 {
		t.Fatalf("信号与 Shutdown 应返回同一错误: %v vs %v", e1, e2)
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("关闭应只执行一次, Stop 计数 = %d", got)
	}
}

// 测试5c：关闭启动后信号句柄必须确被注销（signal.Stop 真实执行的回归测试）。
//
// 确定性论证，全程无 sleep：
//  1. start() 先同步执行 signal.Stop，之后才拉起关闭 goroutine 调用组件 Stop；
//     所以“entered 被关闭”必然发生在这一次 signal.Stop 完成之后。
//  2. 第一发 SIGWINCH 被监听 goroutine 读走（entered 关闭即为该读取已完成的事实），
//     故此刻 sigCh 缓冲必然为空。
//  3. 再发第二发信号，并以 obs 收到第二发作为“该信号已进入 os/signal 串行分发队列”
//     的证据（os/signal 对每次分发持同一把 loop 锁，第二次分发开始即说明第一次的
//     全部 handler 投递早已结束）。
//  4. 此时 sigCh 仍为空 <=> 句柄确被注销。若 signal.Stop 是死代码（回归 bug），
//     第二发信号必然滞留在 sigCh 中，测试稳定失败而不会假通过。
func TestSignalHandleUnregisteredAfterShutdown(t *testing.T) {
	var count atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	m := New(WithSignals(syscall.SIGWINCH), WithStepTimeout(30*time.Second))
	m.Register("block", Func(func(context.Context) error {
		count.Add(1)
		close(entered)
		<-release
		return nil
	}))

	runDone := make(chan error, 1)
	go func() { runDone <- m.Run(context.Background()) }()
	sigCh := waitSigCh(t, m)

	obs := make(chan os.Signal, 4)
	signal.Notify(obs, syscall.SIGWINCH)
	t.Cleanup(func() { signal.Stop(obs) })

	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("kill: %v", err)
	}
	<-entered // 关闭已启动 => signal.Stop(sigCh) 已同步执行完毕
	first := readSig(obs, 5*time.Second)
	if first == nil {
		t.Fatal("第一发 SIGWINCH 未送达观察句柄，测试前提不成立")
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("kill: %v", err)
	}
	second := readSig(obs, 5*time.Second)
	if second == nil {
		t.Fatal("第二发 SIGWINCH 未送达观察句柄，测试前提不成立")
	}
	if n := len(sigCh); n != 0 {
		t.Fatalf("关闭启动后信号句柄未注销：sigCh 中滞留 %d 个信号", n)
	}

	unblock()
	if err := <-runDone; err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("Stop 计数 = %d, want 1", got)
	}
}

// 测试6：重复 Shutdown 幂等 -> 同一错误、Stop 不重复。
func TestRepeatedShutdownIdempotent(t *testing.T) {
	var count atomic.Int64
	m := New()
	m.Register("c", Func(func(context.Context) error {
		count.Add(1)
		return nil
	}))
	e1 := m.Shutdown(context.Background())
	e2 := m.Shutdown(context.Background())
	if e1 != e2 {
		t.Fatalf("重复 Shutdown 应返回同一错误: %v vs %v", e1, e2)
	}
	if e1 != nil {
		t.Fatalf("e1 = %v, want nil", e1)
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("Stop 不应重复执行, 计数 = %d", got)
	}
}

// 测试6b：Shutdown 作为首触发源时 ctx 必须有效（回归 once.Do 同步跑完 shutdownAll
// 导致 ctx.Done 分支不可达的 bug）。
//
// 组件 Stop 会一直阻塞到 unblock()，因此关闭绝不可能在 50ms 内完成——返回
// context.DeadlineExceeded 是确定性结果，不是竞速侥幸；同时后台关闭必须照常完成。
func TestShutdownCtxCancelDoesNotAbortShutdown(t *testing.T) {
	sentinel := errors.New("blocked-stop-failed")
	var count atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	m := New(WithStepTimeout(30 * time.Second))
	m.Register("block", Func(func(context.Context) error {
		count.Add(1)
		close(entered)
		<-release
		return sentinel
	}))

	// 首触发源就是 Shutdown：ctx 必须真的生效。
	shCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := m.Shutdown(shCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx 在关闭完成前被取消时，Shutdown 应返回 context.DeadlineExceeded，实得 %v", err)
	}

	// ctx 提前返回不等于关闭被放弃：后台必须仍在推进。
	<-entered

	waiter := make(chan error, 1)
	go func() { waiter <- m.Shutdown(context.Background()) }()
	unblock()

	got := <-waiter
	if !errors.Is(got, sentinel) {
		t.Fatalf("其余等待者应收到完整聚合错误 %v，实得 %v", sentinel, got)
	}
	if again := m.await(); !errors.Is(again, sentinel) {
		t.Fatalf("await 应复用同一聚合错误，实得 %v", again)
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("Stop 只应执行一次, 计数 = %d", n)
	}
}

// 测试7a：Register 四类 panic 场景。
func TestRegisterPanics(t *testing.T) {
	c := Func(func(context.Context) error { return nil })

	m := New()
	mustPanic(t, "empty name", func() { m.Register("", c) })
	mustPanic(t, "nil component", func() { m.Register("x", nil) })
	m.Register("dup", c)
	mustPanic(t, "duplicate name", func() { m.Register("dup", c) })

	mustPanic(t, "after shutdown", func() {
		mm := New()
		mm.Register("a", c)
		_ = mm.Shutdown(context.Background())
		mm.Register("late", c)
	})
}

// 测试7b：Components 返回副本，修改不影响内部状态。
func TestComponentsReturnsCopy(t *testing.T) {
	c := Func(func(context.Context) error { return nil })
	m := New()
	m.Register("a", c)
	m.Register("b", c)

	names := m.Components()
	if strings.Join(names, ",") != "a,b" {
		t.Fatalf("names = %v, want [a b]", names)
	}
	names[0] = "mutated"
	again := m.Components()
	if again[0] != "a" {
		t.Fatalf("副本修改污染了内部状态: %v", again)
	}
}

// fakeCloseable 带真实的 Close() error 方法，模拟 cache 内部依赖那一类
// 有返回值 Close 的资源，用于验证桥接的是方法本身而不是 atomic 模拟。
type fakeCloseable struct {
	mu     sync.Mutex
	closed bool
}

func (f *fakeCloseable) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeCloseable) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// 测试9：兼容映射冒烟 -> http.Server Shutdown 方法值、Close() error 式闭包。
func TestCompatibilityAdapters(t *testing.T) {
	// 监听后于后台 goroutine 运行，Shutdown 应能干净地关闭它。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{}
	go func() { _ = srv.Serve(ln) }()
	time.Sleep(30 * time.Millisecond) // 等 Serve 进入监听循环

	dep := &fakeCloseable{}
	m := New()
	m.Register("http", Func(srv.Shutdown))
	m.Register("store", Func(func(context.Context) error { return dep.Close() }))

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if !dep.wasClosed() {
		t.Fatal("Close() error 式组件未被调用")
	}
}
