package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Spec 是批发所需的速率规格，由 core 随每次 Wholesale 调用下发。
// 配置单一来源是 core 的 Option——后端不自带速率配置，杜绝双配置源。
type Spec struct {
	// Rate 为 Per 窗口内 Rate 个令牌，Rate > 0。
	Rate int
	// Per 为速率窗口，Per > 0。
	Per time.Duration
	// Burst 为桶容量（突发容忍），Burst >= 1。
	Burst int
	// IdleRetention 由 core 以 WithIdleRetention 填充下发：需要回收状态的
	// 后端（Memory）使用它；GCRA 类后端忽略（EX 过期内建）。
	IdleRetention time.Duration
}

// GrantMode 声明授予语义。
type GrantMode int

const (
	// GrantBestEffort 租约批发：能授多少授多少，granted ∈ [0, want]。
	GrantBestEffort GrantMode = iota
	// GrantAllOrNothing 精确扣减：不足额则拒绝且不扣减（granted ∈ {0, want}），
	// 供配额/计费等不可蒸发场景。
	GrantAllOrNothing
)

// Backend 是令牌批发通道，实现者必须无状态（不自行配置速率），
// 只回答"这次按 spec 能租多少"。所有零售逻辑在 core 层。
//
// 实现者可选实现 io.Closer——仅用于释放连接等资源，与令牌归还无关
// （本包不做 giveback，未用租约靠"批量小 + 后端过期"自然消化）。
//
// 错误契约（实现者必须遵守，对齐 lock.Backend 错误契约三条）：
//   - 后端自身不可用（连接失败、服务宕机等）时，必须将错误包装为
//     ErrBackendUnavailable（fmt.Errorf("%w: %v", ...)），核心据此执行
//     FailPolicy 兜底；
//   - 其余错误（参数错、命令级错、Lua 运行错误）原样返回，核心原样
//     透传，不兜底；
//   - ctx 取消/超时产生的错误必须原样返回 ctx.Err()，不得包装为
//     ErrBackendUnavailable。
type Backend interface {
	// Wholesale 为 key 按 spec 申请 want 个令牌。
	// 返回 (granted, retryAfter, err)：
	//   - granted ∈ [0, want]，语义由 mode 决定；
	//   - 未足额时 retryAfter 提示最早重试时刻（足额可为 0）；
	//   - 后端不可用必须返回包装了 ErrBackendUnavailable 的错误。
	Wholesale(ctx context.Context, key string, want int, spec Spec, mode GrantMode) (granted int, retryAfter time.Duration, err error)
}

// Memory 返回内置单机后端：自持 per-key 惰性补充桶（状态在返回的实例内，
// 多个 Limiter 共享同一实例即共享配额；不共享即各自独立）。
//
// 授予语义：BestEffort 下 granted = min(want, floor(存量))；
// AllOrNothing 下存量不足 granted = 0 且不扣减（防配额蒸发）。
// 闲置回收：桶在 Wholesale 时惰性判断 idle 并重建满桶（超过
// Spec.IdleRetention 未访问即重置），不建后台协程。
func Memory() Backend { return newMemoryBackend(systemClock{}) }

// newMemoryBackend 允许内部测试注入可控时钟。
func newMemoryBackend(c Clock) *memoryBackend {
	return &memoryBackend{clock: c, buckets: make(map[string]*tokenBucket)}
}

// memoryBackend 是 Memory() 的实现：进程内令牌桶注册表。
type memoryBackend struct {
	clock Clock

	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

// Wholesale 实现 Backend。本后端零网络，唯一错误出口是 ctx 取消透传
// （契约三条之"其余错误原样返回"的特例：ctx 错误不得包装为不可用）。
func (m *memoryBackend) Wholesale(ctx context.Context, key string, want int, spec Spec, mode GrantMode) (int, time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}

	now := m.clock.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	b, ok := m.buckets[key]
	// 惰性闲置回收：超过 IdleRetention 未访问的桶直接重置为满桶重建
	// （等价于窗口耗尽后重新积累，无常驻回收需求）。
	if ok && spec.IdleRetention > 0 && now.Sub(b.idleAt) > spec.IdleRetention {
		ok = false
	}
	if !ok {
		b = newTokenBucket(now, spec.Burst)
		m.buckets[key] = b
	}
	b.idleAt = now
	granted, retryAfter := b.take(now, want, spec, mode)
	return granted, retryAfter, nil
}
