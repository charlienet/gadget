package breaker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestBreakerStateMachine 验证熔断器三态状态机：
// 连续失败达阈值 → Open（快速失败）→ 冷却 → HalfOpen → 成功 → Closed。
// （自 redis/breaker_internal_test.go 迁移，适配 New/Option API）
func TestBreakerStateMachine(t *testing.T) {
	b := New(WithThreshold(3), WithCooldown(50*time.Millisecond))
	netErr := errors.New("dial tcp: connection refused")

	// Closed：放行
	if err := b.Allow(); err != nil {
		t.Fatalf("Closed 应放行，got %v", err)
	}

	// 连续失败 2 次（未达阈值 3）：仍 Closed
	b.Fail(netErr)
	b.Fail(netErr)
	if err := b.Allow(); err != nil {
		t.Fatalf("失败 2 次未达阈值，应仍放行，got %v", err)
	}

	// 第 3 次失败：进入 Open（快速失败，原样返回 lastErr）
	b.Fail(netErr)
	if err := b.Allow(); err == nil {
		t.Fatal("Open 应快速失败（返回 lastErr）")
	} else if !errors.Is(err, netErr) {
		t.Fatalf("Open 应返回最近一次错误，got %v", err)
	} else if err != netErr {
		t.Fatalf("拒绝应原样返回 lastErr（禁止包装/哨兵），got %v", err)
	}

	// 冷却期内：仍快速失败
	if err := b.Allow(); err == nil {
		t.Fatal("冷却期内应仍快速失败")
	}

	// 冷却结束：转 HalfOpen 并放行探测
	time.Sleep(60 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatalf("冷却结束应放行探测，got %v", err)
	}

	// 探测成功：闭合
	b.Success()
	if err := b.Allow(); err != nil {
		t.Fatalf("探测成功应闭合恢复，got %v", err)
	}

	// 成功重置计数
	b.Success()
	b.Success()
	if err := b.Allow(); err != nil {
		t.Fatalf("成功后应仍放行，got %v", err)
	}
}

// TestBreakerHalfOpenFail 验证半开探测失败回 Open（重置冷却）。
func TestBreakerHalfOpenFail(t *testing.T) {
	b := New(WithThreshold(2), WithCooldown(30*time.Millisecond))
	netErr := errors.New("connection refused")

	b.Fail(netErr)
	b.Fail(netErr) // Open

	time.Sleep(40 * time.Millisecond) // 冷却结束
	if err := b.Allow(); err != nil {
		t.Fatalf("应放行探测，got %v", err)
	}

	// 探测失败：回 Open，重置冷却（再次进入冷却期）
	b.Fail(netErr)
	if err := b.Allow(); err == nil {
		t.Fatal("探测失败后应回 Open 快速失败")
	}

	// 重新冷却后再次半开
	time.Sleep(40 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatalf("第二次冷却结束应放行探测，got %v", err)
	}
}

// TestBreakerHalfOpenSingleFlight 验证半开探测单飞：
// 并发 Allow 时同时只放行一个探测请求，其余快速失败。
func TestBreakerHalfOpenSingleFlight(t *testing.T) {
	b := New(WithThreshold(3), WithCooldown(20*time.Millisecond))
	netErr := errors.New("connection refused")

	b.Fail(netErr)
	b.Fail(netErr)
	b.Fail(netErr) // Open
	time.Sleep(30 * time.Millisecond)

	const concurrency = 20
	var wg sync.WaitGroup
	allowed := 0
	var mu sync.Mutex

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.Allow(); err == nil {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != 1 {
		t.Fatalf("半开并发应只放行 1 个探测请求，实际 %d", allowed)
	}
}

// TestBreakerConcurrentMixed 混合并发操作压测（配合 -race 验证并发安全）。
func TestBreakerConcurrentMixed(t *testing.T) {
	b := New(WithThreshold(3), WithCooldown(5*time.Millisecond))
	boom := errors.New("boom")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				switch (i + j) % 4 {
				case 0:
					if err := b.Allow(); err == nil {
						b.Report(nil)
					}
				case 1:
					if err := b.Allow(); err == nil {
						b.Report(boom)
					}
				case 2:
					_, _ = Execute(b, func() (int, error) { return j, boom })
				case 3:
					_ = b.State()
					b.Fail(boom)
					b.Success()
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestReportSuccess 验证 Report 分支一（err == nil → Success）：
// 清零计数；半开探测成功 → Closed。
func TestReportSuccess(t *testing.T) {
	b := New(WithThreshold(2), WithCooldown(time.Second))
	e := errors.New("boom")

	b.Report(e)   // failures=1
	b.Report(nil) // 清零
	if err := b.Allow(); err != nil {
		t.Fatalf("Report(nil) 应重置失败计数，got %v", err)
	}
	b.Report(e)
	b.Report(e) // 达阈值 → Open
	if got := b.State(); got != Open {
		t.Fatalf("计数错误达阈值应进入 Open，got %v", got)
	}

	// 半开探测成功自动闭合
	b2 := New(WithThreshold(1), WithCooldown(20*time.Millisecond))
	b2.Report(e) // Open
	time.Sleep(30 * time.Millisecond)
	if err := b2.Allow(); err != nil {
		t.Fatalf("冷却结束应放行探测，got %v", err)
	}
	b2.Report(nil)
	if got := b2.State(); got != Closed {
		t.Fatalf("半开探测成功应闭合，got %v", got)
	}
}

// TestReportCountedFailure 验证 Report 分支二（classifier(err) == true → Fail）：
// 计入连续失败、达阈值触发；半开探测计入失败 → 回 Open 重置冷却。
func TestReportCountedFailure(t *testing.T) {
	counted := errors.New("connection refused")
	b := New(WithThreshold(2), WithCooldown(30*time.Millisecond),
		WithClassifier(func(err error) bool { return errors.Is(err, counted) }))

	b.Report(counted)
	if got := b.State(); got != Closed {
		t.Fatalf("未达阈值应仍 Closed，got %v", got)
	}
	b.Report(counted) // 达阈值 → Open
	if got := b.State(); got != Open {
		t.Fatalf("达阈值应进入 Open，got %v", got)
	}

	time.Sleep(40 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatalf("冷却结束应放行探测，got %v", err)
	}
	b.Report(counted) // 探测失败：回 Open 重置冷却
	if got := b.State(); got != Open {
		t.Fatalf("半开探测失败应回 Open，got %v", got)
	}
	if err := b.Allow(); err == nil {
		t.Fatal("回 Open 后冷却已重置，应快速失败")
	}
}

// TestReportNeutralHalfOpenCloses 验证 Report 分支三（重点，最易迁移丢失）：
// 半开探测 + 非计数（中性）错误 → 服务可达 → Closed 恢复并清零计数。
// 对应原 redis/breaker.go:151-159。
func TestReportNeutralHalfOpenCloses(t *testing.T) {
	counted := errors.New("dial tcp: connection refused")
	neutral := errors.New("WRONGTYPE Operation against a key")
	b := New(WithThreshold(2), WithCooldown(30*time.Millisecond),
		WithClassifier(func(err error) bool { return errors.Is(err, counted) }))

	b.Report(counted)
	b.Report(counted) // Open
	if err := b.Allow(); err == nil {
		t.Fatal("Open 应快速失败")
	}

	time.Sleep(40 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatalf("冷却结束应放行探测，got %v", err)
	}
	if got := b.State(); got != HalfOpen {
		t.Fatalf("应处于 HalfOpen，got %v", got)
	}

	// 中性错误（如命令级错误）：服务可达 → 闭合恢复
	b.Report(neutral)
	if got := b.State(); got != Closed {
		t.Fatalf("半开 + 中性错误应 Closed，got %v", got)
	}
	// 闭合后探测标记已释放：正常放行
	if err := b.Allow(); err != nil {
		t.Fatalf("闭合后应正常放行，got %v", err)
	}
	// 计数已清零：再计 1 次失败（阈值 2）不触发熔断
	b.Report(counted)
	if err := b.Allow(); err != nil {
		t.Fatalf("清零后再计 1 次失败不应触发熔断，got %v", err)
	}
}

// TestReportNeutralKeepsClosedCount 锁定与原 redis 实现逐分支等价的语义：
// Closed 状态下的中性错误既不计入、也不清零既有连续失败计数
// （"连续失败"只看计数错误）。
func TestReportNeutralKeepsClosedCount(t *testing.T) {
	counted := errors.New("refused")
	neutral := errors.New("WRONGTYPE")
	b := New(WithThreshold(3), WithCooldown(time.Second),
		WithClassifier(func(err error) bool { return errors.Is(err, counted) }))

	b.Report(counted)
	b.Report(counted) // failures=2
	b.Report(neutral) // 中性：不影响计数
	b.Report(counted) // failures=3 → Open
	if got := b.State(); got != Open {
		t.Fatalf("中性错误不应清零计数，第 3 次计数失败应触发 Open，got %v", got)
	}
}

// TestReportDefaultClassifier 验证默认 Classifier：所有非 nil 错误计为失败。
func TestReportDefaultClassifier(t *testing.T) {
	b := New(WithThreshold(2), WithCooldown(time.Second))

	b.Report(errors.New("network down")) // 任意类型错误均计入
	b.Report(errors.New("bad payload"))  // → 达阈值 Open
	if got := b.State(); got != Open {
		t.Fatalf("默认应把全部非 nil 错误计为失败，got %v", got)
	}
	if err := b.Allow(); err == nil {
		t.Fatal("Open 应快速失败")
	}
}

// TestExecuteAllowed 验证 Execute 放行路径：fn 执行、返回值原样透传，
// 并自动 Report 结果。
func TestExecuteAllowed(t *testing.T) {
	b := New()
	val, err := Execute(b, func() (string, error) { return "ok", nil })
	if val != "ok" || err != nil {
		t.Fatalf("应透传 fn 返回值，got (%q, %v)", val, err)
	}

	b2 := New(WithThreshold(2), WithCooldown(time.Second))
	wantErr := errors.New("boom")
	val2, err2 := Execute(b2, func() (int, error) { return 7, wantErr })
	if val2 != 7 || err2 != wantErr {
		t.Fatalf("返回值应原样透传，got (%v, %v)", val2, err2)
	}
	// 自动记录：默认分类计入失败，第二次达阈值 → Open
	if _, err := Execute(b2, func() (int, error) { return 0, wantErr }); err != wantErr {
		t.Fatalf("第二次应透传错误，got %v", err)
	}
	if got := b2.State(); got != Open {
		t.Fatalf("Execute 应自动 Report 计数（达阈值 Open），got %v", got)
	}
}

// TestExecuteRejected 验证 Execute 拒绝路径：返回（零值, lastErr）
// 且 lastErr 原样返回，fn 不执行。
func TestExecuteRejected(t *testing.T) {
	b := New(WithThreshold(1), WithCooldown(time.Hour))
	lastErr := errors.New("dial tcp: connection refused")
	if _, err := Execute(b, func() (int, error) { return 0, lastErr }); err != lastErr {
		t.Fatalf("首次失败应透传 lastErr，got %v", err)
	}
	if got := b.State(); got != Open {
		t.Fatalf("阈值 1 应进入 Open，got %v", got)
	}

	called := false
	val, err := Execute(b, func() (int, error) {
		called = true
		return 42, nil
	})
	if called {
		t.Fatal("拒绝时 fn 不应执行")
	}
	if val != 0 {
		t.Fatalf("拒绝应返回零值，got %v", val)
	}
	if err != lastErr {
		t.Fatalf("拒绝应原样返回 lastErr（禁止包装/哨兵），got %v", err)
	}
}

// TestExecuteHalfOpenRecovery 验证 Execute 在半开探测下自动记录：
// 探测成功 → Closed 恢复。
func TestExecuteHalfOpenRecovery(t *testing.T) {
	b := New(WithThreshold(1), WithCooldown(30*time.Millisecond))
	b.Report(errors.New("boom")) // Open
	time.Sleep(40 * time.Millisecond)

	val, err := Execute(b, func() (int, error) { return 1, nil })
	if val != 1 || err != nil {
		t.Fatalf("探测应放行并透传成功，got (%v, %v)", val, err)
	}
	if got := b.State(); got != Closed {
		t.Fatalf("探测成功应自动闭合，got %v", got)
	}
}

// TestExecutePanic 验证 panic 穿透且不计数：fn panic 直接穿透，
// 熔断器不做任何记录（阈值 1 都不触发）。
func TestExecutePanic(t *testing.T) {
	b := New(WithThreshold(1), WithCooldown(time.Hour))
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("panic 应直接穿透 Execute")
			}
		}()
		_, _ = Execute(b, func() (int, error) { panic("boom") })
	}()
	if got := b.State(); got != Closed {
		t.Fatalf("panic 不应计数（阈值 1，计一次即 Open），got %v", got)
	}
	if err := b.Allow(); err != nil {
		t.Fatalf("panic 后应仍正常放行，got %v", err)
	}
}

// TestExecutePanicHalfOpenLeak 锁定文档化行为：半开探测中 fn panic
// 会滞留探测标记（后续 Allow 持续拒绝），调用方自行 Report 可解除。
func TestExecutePanicHalfOpenLeak(t *testing.T) {
	b := New(WithThreshold(1), WithCooldown(30*time.Millisecond))
	b.Report(errors.New("boom")) // Open
	time.Sleep(40 * time.Millisecond)

	func() {
		defer func() { _ = recover() }()
		_, _ = Execute(b, func() (int, error) { panic("boom") })
	}()

	if got := b.State(); got != HalfOpen {
		t.Fatalf("探测 panic 后应仍为 HalfOpen，got %v", got)
	}
	if err := b.Allow(); err == nil {
		t.Fatal("探测标记滞留：后续 Allow 应持续拒绝（文档化泄漏，fn 应保证不 panic）")
	}

	// 调用方感知 panic 并自行补报后可恢复
	b.Report(nil)
	if got := b.State(); got != Closed {
		t.Fatalf("补报后应恢复 Closed，got %v", got)
	}
	if err := b.Allow(); err != nil {
		t.Fatalf("恢复后应正常放行，got %v", err)
	}
}

// TestStateAndString 验证 State() 快照（不触发惰性转换）与 String()。
func TestStateAndString(t *testing.T) {
	b := New(WithThreshold(1), WithCooldown(20*time.Millisecond))
	if got := b.State(); got != Closed {
		t.Fatalf("初始应为 Closed，got %v", got)
	}

	b.Fail(errors.New("boom")) // 阈值 1 → Open
	if got := b.State(); got != Open {
		t.Fatalf("达阈值应为 Open，got %v", got)
	}

	// 纯快照：冷却结束也不改变状态（惰性转换只发生在 Allow）
	time.Sleep(30 * time.Millisecond)
	if got := b.State(); got != Open {
		t.Fatalf("State() 不应触发 Open→HalfOpen 惰性转换，got %v", got)
	}

	if err := b.Allow(); err != nil {
		t.Fatalf("Allow 应触发惰性转换并放行探测，got %v", err)
	}
	if got := b.State(); got != HalfOpen {
		t.Fatalf("冷却后 Allow 应转 HalfOpen，got %v", got)
	}

	b.Success()
	if got := b.State(); got != Closed {
		t.Fatalf("探测成功应回 Closed，got %v", got)
	}

	if Closed.String() != "closed" || Open.String() != "open" || HalfOpen.String() != "half-open" {
		t.Fatalf("String() 输出错误：%q %q %q", Closed, Open, HalfOpen)
	}
}
