package redis

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// TestBreakerStateMachine 验证熔断器三态状态机：
// 连续失败达阈值 → Open（快速失败）→ 冷却 → HalfOpen → 成功 → Closed。
func TestBreakerStateMachine(t *testing.T) {
	b := newCircuitBreaker(3, 50*time.Millisecond)
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

	// 第 3 次失败：进入 Open（快速失败）
	b.Fail(netErr)
	if err := b.Allow(); err == nil {
		t.Fatal("Open 应快速失败（返回 lastErr）")
	} else if !errors.Is(err, netErr) {
		t.Fatalf("Open 应返回最近一次错误，got %v", err)
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
	b := newCircuitBreaker(2, 30*time.Millisecond)
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
	b := newCircuitBreaker(3, 20*time.Millisecond)
	netErr := errors.New("connection refused")

	b.Fail(netErr)
	b.Fail(netErr)
	b.Fail(netErr) // Open
	time.Sleep(30 * time.Millisecond) // 冷却结束 → HalfOpen

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

// TestBreakerOnResult 验证 onResult 的错误分类：
// 成功重置、连接类错误计数、命令级错误不计入。
func TestBreakerOnResult(t *testing.T) {
	b := newCircuitBreaker(2, time.Second)
	netErr := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	cmdErr := errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")

	// 命令级错误不计入熔断
	b.onResult(cmdErr)
	b.onResult(cmdErr)
	if err := b.Allow(); err != nil {
		t.Fatalf("命令级错误不应触发熔断，got %v", err)
	}

	// 连接类错误计数
	b.onResult(netErr)
	b.onResult(netErr) // 达阈值 → Open
	if err := b.Allow(); err == nil {
		t.Fatal("连接类错误达阈值应进入 Open")
	}

	// 成功重置并闭合（半开场景）
	time.Sleep(50 * time.Millisecond)
	// 冷却期 1s 未结束，Open 快速失败；直接验证成功重置计数
	b2 := newCircuitBreaker(2, time.Second)
	b2.onResult(netErr)
	b2.Success() // 重置
	b2.Success()
	if err := b2.Allow(); err != nil {
		t.Fatalf("成功应重置失败计数，got %v", err)
	}
}
