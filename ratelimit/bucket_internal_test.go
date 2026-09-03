package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testSpec() Spec {
	return Spec{Rate: 10, Per: time.Second, Burst: 10, IdleRetention: time.Minute}
}

func TestTokenBucketLazyRefill(t *testing.T) {
	t0 := time.Unix(1_000, 0)
	b := newTokenBucket(t0, 10)
	spec := testSpec()

	// 静止时刻：满桶 10。
	if g, r := b.take(t0, 3, spec, GrantBestEffort); g != 3 || r != 0 {
		t.Fatalf("满桶授予异常: %d/%v", g, r)
	}
	if b.tokens != 7 {
		t.Fatalf("扣减后应为 7，got %v", b.tokens)
	}

	// 推进 200ms：补充 2 个，存量封顶检查前的中间值 9。
	got, _ := b.take(t0.Add(200*time.Millisecond), 20, spec, GrantBestEffort)
	if got != 9 {
		t.Fatalf("9.0 存量应授予 9（floor），got %d", got)
	}
	if b.tokens != 0 {
		t.Fatalf("应清零，got %v", b.tokens)
	}

	// 长时间推进：回补封顶 Burst。
	b.refill(t0.Add(2*time.Hour), spec)
	if b.tokens != 10 {
		t.Fatalf("回补应封顶 Burst=10，got %v", b.tokens)
	}
}

func TestTokenBucketBestEffortFloorNoEvaporation(t *testing.T) {
	t0 := time.Unix(1_000, 0)
	b := newTokenBucket(t0, 10)
	b.tokens = 0 // 从空桶开始，用时间造出小数存量
	spec := testSpec()

	// 270ms × 10/s = 2.7 个令牌。
	b.refill(t0.Add(270*time.Millisecond), spec)
	if diff := b.tokens - 2.7; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("期望 2.7，got %v", b.tokens)
	}
	// BestEffort 按 floor 裁剪：授予 2，扣减量 == 返回量，余 0.7。
	g, _ := b.take(t0.Add(270*time.Millisecond), 5, spec, GrantBestEffort)
	if g != 2 {
		t.Fatalf("floor 裁剪应授予 2，got %d", g)
	}
	if diff := b.tokens - 0.7; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("小数残留应为 0.7（不蒸发），got %v", b.tokens)
	}
}

func TestTokenBucketAllOrNothingNoDeductOnShort(t *testing.T) {
	t0 := time.Unix(1_000, 0)
	b := newTokenBucket(t0, 10)
	b.tokens = 2.5
	spec := testSpec()

	// 不足额：granted=0，存量原样保留，retryAfter 提示补足差额的时长。
	g, retry := b.take(t0, 3, spec, GrantAllOrNothing)
	if g != 0 {
		t.Fatalf("AllOrNothing 不足额必须 granted=0，got %d", g)
	}
	if b.tokens != 2.5 {
		t.Fatalf("拒绝不得扣减（防蒸发），got %v", b.tokens)
	}
	if retry <= 0 || retry > time.Second {
		t.Fatalf("retryAfter 应在 (0,1s] 区间，got %v", retry)
	}

	// 恰足额：放行且精确扣减。
	if g, _ := b.take(t0, 2, spec, GrantAllOrNothing); g != 2 || b.tokens != 0.5 {
		t.Fatalf("足额应扣 2 剩 0.5，got %d/%v", g, b.tokens)
	}
}

func TestTokenBucketClockRewindProtection(t *testing.T) {
	t0 := time.Unix(1_000, 0)
	b := newTokenBucket(t0, 10)
	b.tokens = 1
	spec := testSpec()

	// 时钟回拨：不补充、不清零、不报错（对齐 GCRA tat=max(tat,now)）。
	b.refill(t0.Add(-time.Hour), spec)
	if b.tokens != 1 || !b.last.Equal(t0) {
		t.Fatalf("回拨应无操作，got %v/%v", b.tokens, b.last)
	}
}

func TestMemoryBackendCtxPassthrough(t *testing.T) {
	m := newMemoryBackend(newFakeClock())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := m.Wholesale(ctx, "k", 1, testSpec(), GrantBestEffort)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 取消必须原样透传（不得包装为不可用），got %v", err)
	}
	if errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("ctx 错误不得含 ErrBackendUnavailable，got %v", err)
	}
}

func TestMemoryBackendIdleReset(t *testing.T) {
	clock := newFakeClock()
	m := newMemoryBackend(clock)
	spec := testSpec() // IdleRetention = 1m
	ctx := context.Background()

	// 耗尽存量。
	if g, _, _ := m.Wholesale(ctx, "k", 10, spec, GrantBestEffort); g != 10 {
		t.Fatalf("满桶应授予 10，got %d", g)
	}
	// 短闲置（未超 IdleRetention）：不重置，仅按经过时间惰性回补。
	clock.Advance(500 * time.Millisecond)
	if g, _, _ := m.Wholesale(ctx, "k", 10, spec, GrantBestEffort); g != 5 {
		t.Fatalf("500ms 仅回补 5 个令牌，got %d", g)
	}
	// 长闲置（超 IdleRetention）：整桶重建为满，授予量封顶 Burst。
	clock.Advance(2 * time.Minute)
	if g, _, _ := m.Wholesale(ctx, "k", 10, spec, GrantBestEffort); g != 10 {
		t.Fatalf("闲置重置后应满桶可授予 10，got %d", g)
	}
}

func TestMemoryBackendSharedBetweenLimiters(t *testing.T) {
	clock := newFakeClock()
	m := newMemoryBackend(clock)
	specBurst := WithBurst(10)

	l1 := New(m, WithClock(clock), WithRate(10, time.Second), specBurst)
	l2 := New(m, WithClock(clock), WithRate(10, time.Second), specBurst)
	defer l1.Close()
	defer l2.Close()

	// 两个 Limiter 共享同一 Memory 实例即共享配额：合计放行不超过桶量。
	ctx := context.Background()
	passed := 0
	for i := 0; i < 15; i++ {
		lim := l1
		if i%2 == 1 {
			lim = l2
		}
		if ok, err := lim.Allow(ctx, "shared", 1); ok != (err == nil) {
			t.Fatalf("ok/err 形态不一致: %v %v", ok, err)
		} else if ok {
			passed++
		}
	}
	if passed > 10 {
		t.Fatalf("共享实例的总放行量不得超过桶量 10，got %d", passed)
	}
}
