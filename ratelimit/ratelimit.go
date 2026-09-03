// Package ratelimit 提供后端可插拔的限流器：单机限流与多实例分布式限流，
// 默认采用"远程批发、本地零售"租约模式大幅降低远端调用频率。
// 纯标准库实现，零第三方依赖。
//
// # 设计取舍
//
// v1 只做两种授予语义，不做滑动窗口/漏桶变体、不做 Configure/per-key
// 规则表（不同速率/多租户 = 建多个 Limiter 实例）：
//
//   - 租约模式（默认）：Allow 热路径优先扣减本地租约账存的存量（零网络）；
//     存量不足时向 Backend 批发一整批（批量 = WithLeaseInterval/WithLeaseRatio
//     调节，同 key 并发请求经 in-flight 合并只打一次远端）。本地账本是
//     **纯存量**——remain 只能被批发 granted 注入、被 Allow 扣减，不存在任何
//     按速率的自补充；速率语义 100% 由 Backend 的桶决定，杜绝"本地+远端
//     双重发币"导致的速率翻倍。
//   - 精确模式（WithoutLocalLease）：每次 Allow/Wait 直接以
//     GrantAllOrNothing 调用后端，严格全局配额；不足额拒绝且不扣减，
//     供配额/计费等不可蒸发场景。无本地账本、无后台协程。
//
// 明确不做：giveback 租约归还协议（实例崩溃遗留的未用租约靠"批量小 +
// 后端状态过期"自然消化，浪费上界 = 实例数 × 批量）；retry/breaker 组合
// 装饰层；metrics 出口。
//
// # 突发上界披露（选租约模式的知情项）
//
// 租约模式下各实例本地持有整批租约，全局瞬时突发上界为
// **(实例数 + 1) × Burst**：各实例本地存量之和（至多 实例数 × Burst，
// want 被 clamp 到 Burst）叠加远端桶自身的突发容量（Burst）。
// 且后端状态按 ~burst/rate 量级自然过期/回补，周期性将远端重置为满桶——
// 实际长期速率仍严格受 Spec 约束，但短窗口的放行量可超过单机视角。
// 要求全局严格配额时用 WithoutLocalLease 精确模式。
//
// # 错误契约（分诊表）
//
// Allow 的结果按错误来源分诊（Wait 复用同一分诊，超限外的错误一律终止
// 循环返回，不继续等待）：
//
//	来源                         Allow 结果
//	本地存量不足/静默期/批发后仍不足  (false, *ExceededError)    errors.Is(err, ErrExceeded)
//	ctx 取消/超时                  (false, ctx.Err())          透传，不进 FailPolicy
//	后端不可用 ErrBackendUnavailable  FailOpen: (true, err) 双可判 / FailClosed: (false, err)
//	命令级其他错误（含 Lua 运行错误）  (false, err) 原样透传，不兜底（防配置错误被掩盖）
//	Limiter 已 Close               (false, ErrClosed)          不进 FailPolicy、不触后端
//
// FailOpen 兜底放行返回的 err 同时满足 errors.Is(err, ErrFailOpen) 与
// errors.Is(err, ErrBackendUnavailable)——放行但可感知（对齐
// redis/ratelimit.go fallbackRateResult 先例）。
//
// # 并发说明
//
// Limiter 全部方法并发安全。锁纪律：per-key 账本条目各自持一把互斥锁，
// 临界区内只做三件事——判存量、判静默期、登记/消费 pending，**绝不做
// 网络等待**；批发在途期间，同 key 存量充足的热路径请求与其他 key 的
// 请求均不被阻塞。批发用内部 ctx（context.WithTimeout(context.Background(),
// WithBackendTimeout)），不随单个请求 ctx 取消而殃及同批共享者。
// Backend 在 Wholesale 中 panic 时 panic 继续穿透（本包不 recover）；
// panic 中断当次批发前，leader 路径会清理在途状态并向等待者广播错误
// （等待者收到"批发被中断"错误原样透传，不死等）。
//
// 典型用法（单机）：
//
//	limiter := ratelimit.New(ratelimit.Memory(),
//		ratelimit.WithRate(100, time.Minute),
//		ratelimit.WithBurst(200),
//	)
//	defer limiter.Close()
//
//	if ok, err := limiter.Allow(ctx, "user:42", 1); !ok {
//		if errors.Is(err, ratelimit.ErrExceeded) {
//			return errors.New("请求过于频繁")
//		}
//		return err
//	}
package ratelimit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Clock 是时间源抽象，所有内部时间读取均经它（WithClock 注入可控时钟后
// 测试免 sleep、输出确定）。
type Clock interface {
	Now() time.Time
}

// systemClock 是默认实现：直读系统时间。
type systemClock struct{}

// Now 实现 Clock。
func (systemClock) Now() time.Time { return time.Now() }

// Limiter 是限流器：零售逻辑（本地租约账本、in-flight 批发合并、静默期、
// 错误分诊、兜底策略）集中在本层，Backend 只回答"按 Spec 能租多少"。
//
// 生命周期：New 创建（租约模式下启动单个闲置回收后台协程）；Close 幂等
// 停止后台协程并在 Backend 实现 io.Closer 时释放其资源；Close 后
// Allow/Wait 返回 ErrClosed。
type Limiter struct {
	backend Backend
	spec    Spec

	policy         FailPolicy
	localLease     bool
	maxWait        time.Duration
	leaseInterval  time.Duration
	leaseRatio     float64
	backendTimeout time.Duration
	logger         *slog.Logger
	clock          Clock

	// after 抽象等待计时：默认真实计时器，测试注入 fake clock 的
	// 可控实现（Wait 的重试等待随之可控）。
	after func(d time.Duration) <-chan time.Time

	// ledger 是本地租约账本（仅租约模式访问）。
	ledger *ledger

	closeOnce sync.Once
	stopped   chan struct{}
	wg        sync.WaitGroup
	closed    atomic.Bool
}

// New 创建限流器。b 为 nil 时 panic（fail-fast，对齐 lock.New 先例）。
// 非法 Option 值一律防御式忽略，保持默认。
func New(b Backend, opts ...Option) *Limiter {
	if b == nil {
		panic("ratelimit: New 需要非 nil Backend")
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	o.finalize()

	l := &Limiter{
		backend:        b,
		spec:           Spec{Rate: o.rate, Per: o.per, Burst: o.burst, IdleRetention: o.idleRetention},
		policy:         o.policy,
		localLease:     o.localLease,
		maxWait:        o.maxWait,
		leaseInterval:  o.leaseInterval,
		leaseRatio:     o.leaseRatio,
		backendTimeout: o.backendTimeout,
		logger:         o.logger,
		clock:          o.clock,
		after:          realAfter,
		ledger:         newLedger(),
		stopped:        make(chan struct{}),
	}
	// 闲置回收协程仅租约模式需要（精确模式无本地账本）；受控退出对齐
	// cache 先例：Once + stopChan + WaitGroup，不加 recover。
	if l.localLease {
		l.wg.Add(1)
		go l.sweepLoop()
	}
	return l
}

// Allow 非阻塞消耗 n 个令牌。
//
// 结果分诊（详见包 doc 错误契约表）：
//   - 放行 → (true, nil)；
//   - 超限 → (false, *ExceededError)，判定走 errors.Is(err, ErrExceeded)；
//   - FailOpen 兜底放行 → (true, 包装了 ErrFailOpen 的错误)；
//   - 参数错误（key 为空、n <= 0、n > Burst）→ (false, ErrInvalidArgument)，
//     fail-fast 先于一切路径；
//   - 单次 Allow 至多发起一次批发，不循环打远端。
func (l *Limiter) Allow(ctx context.Context, key string, n int) (bool, error) {
	if err := l.checkArgs(key, n); err != nil {
		return false, err
	}
	if l.closed.Load() {
		return false, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !l.localLease {
		return l.strictAllow(ctx, key, n)
	}
	return l.leaseAllow(ctx, key, n)
}

// strictAllow 精确模式：直接向后端 AllOrNothing 扣减，无本地缓存。
func (l *Limiter) strictAllow(ctx context.Context, key string, n int) (bool, error) {
	granted, retryAfter, err := l.backend.Wholesale(ctx, key, n, l.spec, GrantAllOrNothing)
	if err != nil {
		// 精确模式每次请求即一次独立批发：兜底事件日志在此记一条
		// （本路径无共享、无锁，N-F 与租约模式规则一致）。
		l.logFailOpenFallback(ctx, err, key, n)
		return l.triageBackendErr(ctx, err, key, n)
	}
	if granted >= n {
		return true, nil
	}
	// AllOrNothing 契约下 granted ∈ {0, want}；防御小于的返回值。
	return false, &ExceededError{Key: key, N: n, RetryAfter: retryAfter}
}

// Wait 阻塞等待 n 个令牌。四出口：
//
//   - 成功 → nil；
//   - ctx 取消/超时 → ctx.Err()（不伪装成超限，对齐 redis/ratelimit.go
//     waitLoop 先例）；
//   - 总等待超出 WithMaxWait → ErrExceeded 语义错误（*ExceededError）；
//   - 后端不可用 → 立即按 FailPolicy 返回包装错误并终止循环，不继续
//     等待（Open：ErrFailOpen+ErrBackendUnavailable 双可判；Closed：拒绝
//     错误）——两种策略都返回错误、都不进死循环，防吞错忙等。
//
// 超限被拒后按 max(RetryAfter, 1ms) 定时等待再重试（对齐 waitLoop）。
func (l *Limiter) Wait(ctx context.Context, key string, n int) error {
	if err := l.checkArgs(key, n); err != nil {
		return err
	}
	if l.closed.Load() {
		return ErrClosed
	}
	deadline := l.clock.Now().Add(l.maxWait)
	for {
		ok, err := l.Allow(ctx, key, n)

		var xe *ExceededError
		switch {
		case ok && err == nil:
			return nil

		case errors.As(err, &xe):
			// 唯一可续循环的出口：被拒后按建议时长等待。
			wait := xe.RetryAfter
			if wait <= 0 {
				wait = minWaitInterval
			}
			rem := deadline.Sub(l.clock.Now())
			if rem <= 0 {
				return &ExceededError{Key: key, N: n}
			}
			if wait > rem {
				wait = rem
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-l.after(wait):
			}
			if !l.clock.Now().Before(deadline) {
				return &ExceededError{Key: key, N: n}
			}

		case err != nil:
			// ctx 取消、ErrClosed、命令级错误、后端不可用（Open/Closed
			// 皆然）——立即返回，不继续循环等待。
			return err

		default:
			// 防御：(false, nil) 按契约不可达，按超限语义错误返回避免死循环。
			return &ExceededError{Key: key, N: n}
		}
	}
}

// Close 幂等停止后台 sweeper；Backend 实现 io.Closer 时调用其 Close
// （仅用于释放连接等资源）。Close 后 Allow/Wait 返回 ErrClosed。
func (l *Limiter) Close() error {
	var err error
	l.closeOnce.Do(func() {
		l.closed.Store(true)
		close(l.stopped)
		l.wg.Wait()
		if c, ok := l.backend.(io.Closer); ok {
			err = c.Close()
		}
	})
	return err
}

// checkArgs 参数契约（fail-fast，先于任何路径）：
//   - key 为空或 n <= 0 → ErrInvalidArgument；
//   - n > Burst → ErrInvalidArgument（"本实例任何时刻都无法满足"，
//     不做静默钳制）。
func (l *Limiter) checkArgs(key string, n int) error {
	if key == "" {
		return &invalidError{msg: "ratelimit: key 不能为空"}
	}
	if n <= 0 {
		return &invalidError{msg: "ratelimit: n 必须为正数"}
	}
	if n > l.spec.Burst {
		return &invalidError{msg: "ratelimit: n 超过桶容量，本实例任何时刻都无法满足"}
	}
	return nil
}

// invalidError 包装 ErrInvalidArgument 并携带说明文案。
type invalidError struct{ msg string }

func (e *invalidError) Error() string { return e.msg }
func (e *invalidError) Unwrap() error { return ErrInvalidArgument }

// want 计算租约批发批量：
//
//	want = clamp(round(Rate × LeaseInterval / Per × LeaseRatio), 1, Burst)
func (l *Limiter) want() int {
	target := math.Round(float64(l.spec.Rate) * float64(l.leaseInterval) / float64(l.spec.Per) * l.leaseRatio)
	if target < 1 {
		return 1
	}
	if target > float64(l.spec.Burst) {
		return l.spec.Burst
	}
	return int(target)
}

// nominalDelay 按标称速率估算消耗 n 个令牌的时长，用于后端未给出
// retryAfter 时的超限提示值。
func (l *Limiter) nominalDelay(n int) time.Duration {
	if l.spec.Rate <= 0 {
		return 0
	}
	return time.Duration(float64(l.spec.Per) * float64(n) / float64(l.spec.Rate))
}

// realAfter 是默认计时器（Go 1.23+ 语义下未消费的 time.After 通道可被
// GC 回收，无需显式 Stop）。
func realAfter(d time.Duration) <-chan time.Time {
	return time.After(d)
}
