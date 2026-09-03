package retry

import (
	mrand "math/rand"
	"time"
)

// Backoff 退避策略接口：每次重试前由 Do 调用 NextBackOff 取得等待时长。
//
// 实例非并发安全；多个 goroutine 并发 Do 时请各自构造独立的 Backoff 实例
// （使用默认配置时无需关心，Do 会在每次调用内部创建独立实例）。
type Backoff interface {
	// NextBackOff 返回下一次重试前应等待的时长；Reset 之后首次调用返回
	// 序列的第一个值。返回值 ≤0 时 Do 视为 0（立即重试）。
	NextBackOff() time.Duration

	// Reset 将策略重置回初始状态，下一次 NextBackOff 从头开始。
	// Do 在每次执行入口调用本方法。
	Reset()
}

// fixed 实现固定间隔退避。
type fixed struct {
	d time.Duration
}

// Fixed 返回固定间隔 d 的退避策略。
//
// d <= 0 时 panic（fail-fast，与 lock 包构造期校验惯例一致）。
func Fixed(d time.Duration) Backoff {
	if d <= 0 {
		panic("retry: Fixed 要求 d > 0")
	}
	return &fixed{d: d}
}

// NextBackOff 恒定返回固定间隔。
func (f *fixed) NextBackOff() time.Duration { return f.d }

// Reset 对固定间隔无效果。
func (f *fixed) Reset() {}

// exponential 实现指数增长、上限截断的退避策略。
type exponential struct {
	initial    time.Duration
	multiplier float64
	max        time.Duration
	cur        time.Duration // 下次 NextBackOff 应返回的值
}

// Exponential 返回指数退避策略：首次等待 initial，此后每步
// cur = min(cur*multiplier, max)——先乘后 clamp，杜绝 float64 溢出。
//
// 以下任一条件 panic（fail-fast）：
//   - initial <= 0
//   - multiplier < 1（退避不增长无意义）
//   - max < initial（首值即越界）
func Exponential(initial time.Duration, multiplier float64, max time.Duration) Backoff {
	if initial <= 0 {
		panic("retry: Exponential 要求 initial > 0")
	}
	if multiplier < 1 {
		panic("retry: Exponential 要求 multiplier >= 1")
	}
	if max < initial {
		panic("retry: Exponential 要求 max >= initial")
	}
	return &exponential{
		initial:    initial,
		multiplier: multiplier,
		max:        max,
		cur:        initial,
	}
}

// NextBackOff 返回当前值，随后推进 cur = min(cur*multiplier, max)。
func (e *exponential) NextBackOff() time.Duration {
	v := e.cur
	next := time.Duration(float64(e.cur) * e.multiplier)
	if next > e.max || next <= 0 { // next<=0 双保险：float64 舍入或溢出回绕
		next = e.max
	}
	e.cur = next
	return v
}

// Reset 将序列重置回 initial。
func (e *exponential) Reset() { e.cur = e.initial }

// fullJitter 实现 Full Jitter（AWS 风格）：在 [0, 底层值) 内均匀随机。
type fullJitter struct {
	b Backoff
}

// FullJitter 包装底层策略 b：返回 rand[0, d)，d 为 b 的当前退避值。
// 底层 d <= 0 时返回 0。随机源为 math/rand（Go 1.20+ 自动播种）。
//
// b 为 nil 时 panic（fail-fast，与 Fixed / Exponential 构造期校验一致）。
func FullJitter(b Backoff) Backoff {
	if b == nil {
		panic("retry: FullJitter 要求 b 非 nil")
	}
	return &fullJitter{b: b}
}

// NextBackOff 返回 rand[0, d)。
func (f *fullJitter) NextBackOff() time.Duration {
	d := f.b.NextBackOff()
	if d <= 0 {
		return 0
	}
	return time.Duration(mrand.Int63n(int64(d)))
}

// Reset 重置底层策略。
func (f *fullJitter) Reset() { f.b.Reset() }

// equalJitter 实现 Equal Jitter：一半固定、一半随机。
type equalJitter struct {
	b Backoff
}

// EqualJitter 包装底层策略 b：返回 d/2 + rand[0, d/2)，d 为 b 的当前
// 退避值。底层 d <= 0 时返回 0；d=1ns 时整除截断（half=0，返回 0）。
// 随机源为 math/rand（Go 1.20+ 自动播种）。
//
// b 为 nil 时 panic（fail-fast，与 Fixed / Exponential 构造期校验一致）。
func EqualJitter(b Backoff) Backoff {
	if b == nil {
		panic("retry: EqualJitter 要求 b 非 nil")
	}
	return &equalJitter{b: b}
}

// NextBackOff 返回 d/2 + rand[0, d/2)。
func (e *equalJitter) NextBackOff() time.Duration {
	d := e.b.NextBackOff()
	if d <= 0 {
		return 0
	}
	half := d / 2
	if half <= 0 { // d==1ns：整除截断为 0，无可随机区间
		return 0
	}
	return half + time.Duration(mrand.Int63n(int64(half)))
}

// Reset 重置底层策略。
func (e *equalJitter) Reset() { e.b.Reset() }
