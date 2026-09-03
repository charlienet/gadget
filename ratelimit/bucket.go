package ratelimit

import (
	"math"
	"time"
)

// tokenBucket 是惰性补充令牌桶（仅供 Memory 后端使用）：
// 不在后台推进，读取时按距上次补充的时间差一次性补足并封顶 Burst。
//
// 时钟回拨保护：now <= last 时不做补充（不扣减存量），对齐 GCRA
// tat = max(tat, now) 的语义。
type tokenBucket struct {
	tokens float64   // 当前存量（可为小数，授予时按 floor 裁剪）
	last   time.Time // 上次补充基准时刻
	idleAt time.Time // 上次访问时刻（闲置回收判定，memoryBackend 维护）
}

// newTokenBucket 创建满桶（冷启动即满，无首请求饥饿）。
func newTokenBucket(now time.Time, burst int) *tokenBucket {
	return &tokenBucket{tokens: float64(burst), last: now, idleAt: now}
}

// refill 按时间差惰性补充令牌，封顶 Burst。
func (b *tokenBucket) refill(now time.Time, spec Spec) {
	if !now.After(b.last) {
		return
	}
	elapsed := now.Sub(b.last).Seconds()
	ratePerSec := float64(spec.Rate) / spec.Per.Seconds()
	b.tokens += elapsed * ratePerSec
	if b.tokens > float64(spec.Burst) {
		b.tokens = float64(spec.Burst)
	}
	b.last = now
}

// take 尝试消耗 want 个令牌，返回 (granted, retryAfter)。
//
//   - GrantAllOrNothing：存量 >= want 才扣减；不足额 granted=0 且
//     **不做任何扣减**（防配额蒸发），retryAfter 为补足差额所需时长；
//   - GrantBestEffort：granted = min(want, floor(存量))——小数存量按
//     floor 裁剪，保证扣减量 == 返回量（每次部分授予不蒸发令牌）。
func (b *tokenBucket) take(now time.Time, want int, spec Spec, mode GrantMode) (int, time.Duration) {
	b.refill(now, spec)

	if mode == GrantAllOrNothing {
		if b.tokens >= float64(want) {
			b.tokens -= float64(want)
			return want, 0
		}
		return 0, b.deficitTime(float64(want)-b.tokens, spec)
	}

	granted := int(math.Floor(b.tokens))
	if granted > want {
		granted = want
	}
	if granted > 0 {
		b.tokens -= float64(granted)
		return granted, 0
	}
	// 存量为 0：提示产生下一个整数令牌所需时长（最早重试时刻）。
	return 0, b.deficitTime(1.0-b.tokens, spec)
}

// deficitTime 返回按 spec 速率生成 deficit 个整数令牌所需的时长。
func (b *tokenBucket) deficitTime(deficit float64, spec Spec) time.Duration {
	if deficit <= 0 || spec.Rate <= 0 {
		return 0
	}
	return time.Duration(deficit * float64(spec.Per) / float64(spec.Rate))
}
