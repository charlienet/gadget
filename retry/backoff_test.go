package retry

import (
	"testing"
	"time"
)

// seqBackoff 是测试用确定性策略：按表返回值，表耗尽后重复最后一个；
// resetCnt 记录 Reset 被调用的次数。
type seqBackoff struct {
	values   []time.Duration
	i        int
	resetCnt int
}

func (s *seqBackoff) NextBackOff() time.Duration {
	v := s.values[s.i]
	if s.i < len(s.values)-1 {
		s.i++
	}
	return v
}

func (s *seqBackoff) Reset() {
	s.i = 0
	s.resetCnt++
}

func TestFixedConstant(t *testing.T) {
	b := Fixed(42 * time.Millisecond)
	for i := 0; i < 10; i++ {
		if got := b.NextBackOff(); got != 42*time.Millisecond {
			t.Fatalf("第 %d 次 NextBackOff = %v, want 42ms", i, got)
		}
	}
	b.Reset() // Fixed 的 Reset 无效果
	if got := b.NextBackOff(); got != 42*time.Millisecond {
		t.Fatalf("Reset 后 = %v, want 42ms", got)
	}
}

func TestExponentialSequence(t *testing.T) {
	b := Exponential(100*time.Millisecond, 2, 10*time.Second)
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		3200 * time.Millisecond,
		6400 * time.Millisecond,
		10 * time.Second,
		10 * time.Second,
	}
	for i, w := range want {
		if got := b.NextBackOff(); got != w {
			t.Fatalf("第 %d 步 = %v, want %v", i, got, w)
		}
	}
}

func TestExponentialLargeNStaysWithinMax(t *testing.T) {
	const max = 10 * time.Second
	b := Exponential(time.Millisecond, 2, max)
	for i := 0; i < 100000; i++ {
		got := b.NextBackOff()
		if got < 0 {
			t.Fatalf("第 %d 步出现负值（溢出回绕）: %v", i, got)
		}
		if got > max {
			t.Fatalf("第 %d 步超过 max: %v > %v", i, got, max)
		}
	}
}

func TestExponentialHugeMultiplierNoWrap(t *testing.T) {
	// 1e15 倍率：float64(1s)*1e15 超出 int64 上限，转回 Duration
	// 触发 NextBackOff 中 next<=0 双保险子条件（溢出回绕分支）。
	const max = time.Minute
	b := Exponential(time.Second, 1e15, max)
	if got := b.NextBackOff(); got != time.Second {
		t.Fatalf("首值 = %v, want 1s", got)
	}
	for i := 0; i < 100; i++ {
		got := b.NextBackOff()
		if got < 0 {
			t.Fatalf("第 %d 步出现负值（溢出回绕穿透）: %v", i, got)
		}
		if got > max {
			t.Fatalf("第 %d 步超过 max: %v > %v", i, got, max)
		}
	}
}

func TestExponentialResetRestarts(t *testing.T) {
	b := Exponential(100*time.Millisecond, 2, 10*time.Second)
	b.NextBackOff()
	b.NextBackOff()
	b.Reset()
	if got := b.NextBackOff(); got != 100*time.Millisecond {
		t.Fatalf("Reset 后首值 = %v, want 100ms", got)
	}
	if got := b.NextBackOff(); got != 200*time.Millisecond {
		t.Fatalf("Reset 后第二值 = %v, want 200ms", got)
	}
}

func TestExponentialMultiplierOne(t *testing.T) {
	b := Exponential(time.Second, 1, time.Second)
	for i := 0; i < 5; i++ {
		if got := b.NextBackOff(); got != time.Second {
			t.Fatalf("multiplier=1 第 %d 步 = %v, want 1s", i, got)
		}
	}
}

func TestFullJitterRange(t *testing.T) {
	const d = time.Minute
	b := FullJitter(&seqBackoff{values: []time.Duration{d}})
	for i := 0; i < 1000; i++ {
		got := b.NextBackOff()
		if got < 0 || got >= d {
			t.Fatalf("FullJitter 越界: %v not in [0, %v)", got, d)
		}
	}
}

func TestFullJitterNonPositive(t *testing.T) {
	b := FullJitter(&seqBackoff{values: []time.Duration{0}})
	if got := b.NextBackOff(); got != 0 {
		t.Fatalf("底层 d=0 时 FullJitter = %v, want 0", got)
	}
	b = FullJitter(&seqBackoff{values: []time.Duration{-5}})
	if got := b.NextBackOff(); got != 0 {
		t.Fatalf("底层 d<0 时 FullJitter = %v, want 0", got)
	}
}

func TestEqualJitterRange(t *testing.T) {
	const d = time.Minute
	b := EqualJitter(&seqBackoff{values: []time.Duration{d}})
	for i := 0; i < 1000; i++ {
		got := b.NextBackOff()
		if got < d/2 || got >= d {
			t.Fatalf("EqualJitter 越界: %v not in [%v, %v)", got, d/2, d)
		}
	}
}

func TestEqualJitterNonPositiveAndOneNano(t *testing.T) {
	b := EqualJitter(&seqBackoff{values: []time.Duration{0}})
	if got := b.NextBackOff(); got != 0 {
		t.Fatalf("底层 d=0 时 EqualJitter = %v, want 0", got)
	}
	b = EqualJitter(&seqBackoff{values: []time.Duration{-5}})
	if got := b.NextBackOff(); got != 0 {
		t.Fatalf("底层 d<0 时 EqualJitter = %v, want 0", got)
	}
	// d=1ns：half = 1/2 整除截断为 0，只能返回 0
	b = EqualJitter(&seqBackoff{values: []time.Duration{1}})
	if got := b.NextBackOff(); got != 0 {
		t.Fatalf("底层 d=1ns 时 EqualJitter = %v, want 0（整除截断）", got)
	}
	// d=2ns：half=1，值域 {1}
	b = EqualJitter(&seqBackoff{values: []time.Duration{2}})
	if got := b.NextBackOff(); got != 1 {
		t.Fatalf("底层 d=2ns 时 EqualJitter = %v, want 1", got)
	}
}

func TestJitterResetPropagates(t *testing.T) {
	seq := &seqBackoff{values: []time.Duration{time.Second}}
	FullJitter(seq).Reset()
	if seq.resetCnt != 1 {
		t.Fatalf("FullJitter.Reset 未传递到底层: resetCnt=%d", seq.resetCnt)
	}
	EqualJitter(seq).Reset()
	if seq.resetCnt != 2 {
		t.Fatalf("EqualJitter.Reset 未传递到底层: resetCnt=%d", seq.resetCnt)
	}
}

func TestConstructorPanics(t *testing.T) {
	cases := []struct {
		name string
		fn   func()
	}{
		{"Fixed(0)", func() { Fixed(0) }},
		{"Fixed(负值)", func() { Fixed(-time.Second) }},
		{"Exponential(initial<=0)", func() { Exponential(0, 2, time.Minute) }},
		{"Exponential(multiplier<1)", func() { Exponential(time.Second, 0.5, time.Minute) }},
		{"Exponential(max<initial)", func() { Exponential(time.Minute, 2, time.Second) }},
		{"FullJitter(nil)", func() { FullJitter(nil) }},
		{"EqualJitter(nil)", func() { EqualJitter(nil) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s 未 panic", c.name)
				}
			}()
			c.fn()
		})
	}
}
