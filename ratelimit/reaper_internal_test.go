package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// bucketCount 是包内测试辅助访问器（不新增导出 API）：返回当前桶条目数。
func (m *memoryBackend) bucketCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buckets)
}

// reaperSpec 是带闲置回收阈值的测试规格。
func reaperSpec() Spec {
	return Spec{Rate: 10, Per: time.Second, Burst: 10, IdleRetention: time.Minute}
}

// TestMemoryBackendReapIdleShrinksEntries 回归核心缺陷：无界 key 空间下
// buckets map 条目曾只增不删。推进 idle 期后 reapIdle 必须让 len(buckets)
// 真实下降（冷 key 被 delete，仅保留最近访问的热 key）。
func TestMemoryBackendReapIdleShrinksEntries(t *testing.T) {
	clock := newFakeClock()
	m := newMemoryBackend(clock)
	spec := reaperSpec()
	ctx := context.Background()

	const n = 500
	for i := 0; i < n; i++ {
		if _, _, err := m.Wholesale(ctx, fmt.Sprintf("ip:%d", i), 1, spec, GrantBestEffort); err != nil {
			t.Fatalf("Wholesale(%d): %v", i, err)
		}
	}
	if got := m.bucketCount(); got != n {
		t.Fatalf("建满应有 %d 个条目，got %d", n, got)
	}

	// 推进超过 IdleRetention：全部条目变冷。
	clock.Advance(2 * time.Minute)

	// 热 key：仅再访问前 5 个，刷新其 idleAt。
	for i := 0; i < 5; i++ {
		if _, _, err := m.Wholesale(ctx, fmt.Sprintf("ip:%d", i), 1, spec, GrantBestEffort); err != nil {
			t.Fatalf("hot Wholesale(%d): %v", i, err)
		}
	}

	// 清扫：冷 key 条目被 delete，len 真实下降。
	m.reapIdle(clock.Now(), spec.IdleRetention)
	if got := m.bucketCount(); got != 5 {
		t.Fatalf("reap 后应仅剩 5 个热 key，got %d", got)
	}
}

// TestMemoryBackendReapRebuildEqualsFreshBucket 验证语义保持：闲置未超阈
// 不回收、回收后重建等价全新满桶（不丢租约语义，租约在 ledger 侧）。
func TestMemoryBackendReapRebuildEqualsFreshBucket(t *testing.T) {
	clock := newFakeClock()
	m := newMemoryBackend(clock)
	spec := reaperSpec()
	ctx := context.Background()

	// 耗尽存量。
	if g, _, _ := m.Wholesale(ctx, "k", 10, spec, GrantBestEffort); g != 10 {
		t.Fatalf("满桶应授 10，got %d", g)
	}

	// 短闲置（未超 IdleRetention）：reap 不删条目；仅按经过时间回补（5 个）。
	clock.Advance(500 * time.Millisecond)
	m.reapIdle(clock.Now(), spec.IdleRetention)
	if got := m.bucketCount(); got != 1 {
		t.Fatalf("未超阈不应回收，got %d 条目", got)
	}
	if g, _, _ := m.Wholesale(ctx, "k", 10, spec, GrantBestEffort); g != 5 {
		t.Fatalf("短闲置应仅回补 5，got %d", g)
	}

	// 长闲置：reap 删除条目，下次访问重建为满桶（等价全新，不受历史扣减影响）。
	clock.Advance(2 * time.Minute)
	m.reapIdle(clock.Now(), spec.IdleRetention)
	if got := m.bucketCount(); got != 0 {
		t.Fatalf("超阈应回收，got %d 条目", got)
	}
	if g, _, _ := m.Wholesale(ctx, "k", 10, spec, GrantBestEffort); g != 10 {
		t.Fatalf("回收后重建应等价满桶 10，got %d", g)
	}
}

// TestLimiterSweepOnceReapsMemoryEntries 端到端：Limiter 的 sweepOnce（同一
// tick）驱动 memoryBackend.buckets 条目回收——覆盖无界增长的真实入口。
func TestLimiterSweepOnceReapsMemoryEntries(t *testing.T) {
	clock := newFakeClock()
	mem := newMemoryBackend(clock)
	spec := reaperSpec()
	ctx := context.Background()

	l := New(mem, WithClock(clock), WithRate(10, time.Second), WithBurst(10), WithIdleRetention(time.Minute))
	defer l.Close()

	for i := 0; i < 100; i++ {
		if _, _, err := mem.Wholesale(ctx, fmt.Sprintf("u:%d", i), 1, spec, GrantBestEffort); err != nil {
			t.Fatalf("Wholesale: %v", err)
		}
	}
	if got := mem.bucketCount(); got != 100 {
		t.Fatalf("应建 100 条目，got %d", got)
	}

	// 推进超阈后经 Limiter.sweepOnce 触发回收。
	clock.Advance(2 * time.Minute)
	l.sweepOnce(clock.Now())
	if got := mem.bucketCount(); got != 0 {
		t.Fatalf("sweepOnce 应回收后端条目，got %d", got)
	}
}

// TestMemoryBackendReapIdleConcurrentNoPanic -race 下并发读写 + 回收不 panic、
// 无数据竞争：Wholesale、reapIdle、bucketCount、clock.Advance 全并发。
func TestMemoryBackendReapIdleConcurrentNoPanic(t *testing.T) {
	clock := newFakeClock()
	m := newMemoryBackend(clock)
	spec := reaperSpec()
	ctx := context.Background()

	var wg sync.WaitGroup
	// 并发写入方（不同 key 空间重叠）。
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				key := fmt.Sprintf("c:%d", (base*300+j)%1000)
				if _, _, err := m.Wholesale(ctx, key, 1, spec, GrantBestEffort); err != nil {
					t.Errorf("Wholesale: %v", err)
					return
				}
			}
		}(w)
	}
	// 并发清扫方。
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.reapIdle(clock.Now(), spec.IdleRetention)
				_ = m.bucketCount()
			}
		}()
	}
	// 并发推进时钟方（模拟闲置期滚动）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 200; j++ {
			clock.Advance(time.Second)
		}
	}()

	wg.Wait()
}

// TestMemoryBackendReapIdleDisabledOnZeroRetention 验证 retention<=0 防御：
// 不回收任何条目（与 Spec 禁用语义一致）。
func TestMemoryBackendReapIdleDisabledOnZeroRetention(t *testing.T) {
	clock := newFakeClock()
	m := newMemoryBackend(clock)
	ctx := context.Background()
	spec := reaperSpec()

	for i := 0; i < 10; i++ {
		m.Wholesale(ctx, fmt.Sprintf("z:%d", i), 1, spec, GrantBestEffort)
	}
	clock.Advance(time.Hour)
	m.reapIdle(clock.Now(), 0)
	if got := m.bucketCount(); got != 10 {
		t.Fatalf("retention<=0 不应回收，got %d", got)
	}
}
