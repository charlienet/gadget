package ratelimit

import (
	"errors"
	"fmt"
	"time"
)

// 哨兵错误（全量导出，判定统一走 errors.Is）。
var (
	// ErrExceeded 表示请求被限流（本地存量不足 / 静默期 / 批发后仍不足 /
	// Wait 总等待超出 WithMaxWait）。实际错误值为 *ExceededError（携带
	// Key/N/RetryAfter），通过 Unwrap 挂链本哨兵。
	ErrExceeded = errors.New("ratelimit: rate limit exceeded")

	// ErrBackendUnavailable 表示后端不可用（连接失败、服务宕机等）。
	// Backend 实现者在 Wholesale 中检测到"服务不可用"类错误时必须包装
	// 本哨兵，核心据此触发 FailPolicy 兜底；命令级错误不得包装。
	ErrBackendUnavailable = errors.New("ratelimit: backend unavailable")

	// ErrFailOpen 表示本次放行是后端不可用时 FailOpen 兜底的结果：
	// Allow 返回 (true, err)，err 同时满足 errors.Is(err, ErrFailOpen)
	// 与 errors.Is(err, ErrBackendUnavailable)——放行但可感知。
	ErrFailOpen = errors.New("ratelimit: allowed by fail-open fallback")

	// ErrInvalidArgument 表示参数契约错误（key 为空、n <= 0、n > Burst），
	// fail-fast 返回，不触碰任何后端。
	ErrInvalidArgument = errors.New("ratelimit: invalid argument")

	// ErrClosed 表示 Limiter 已 Close：Allow/Wait 直接拒绝，不进
	// FailPolicy、不触后端。
	ErrClosed = errors.New("ratelimit: limiter is closed")
)

// ExceededError 是超限错误的结构化载体，可经 errors.As 取出，
// RetryAfter 供上层（或 Wait 内部）决定重试等待时长。
type ExceededError struct {
	Key        string        // 限流 key
	N          int           // 本次请求消耗的令牌数
	RetryAfter time.Duration // 建议重试等待时长；0 表示无法预估
}

// Error 实现 error 接口。
func (e *ExceededError) Error() string {
	return fmt.Sprintf("ratelimit: exceeded: key=%q n=%d retry_after=%s", e.Key, e.N, e.RetryAfter)
}

// Unwrap 使 errors.Is(err, ErrExceeded) 恒成立。
func (e *ExceededError) Unwrap() error { return ErrExceeded }

// errBrokenBatch 是 Backend panic 中断当次批发时，广播给等待者的内部错误
// 占位（哨兵非导出：它不是供调用方分诊的契约错误，仅保证 followers 不死等）。
// 走"命令级错误原样透传"分诊行——不包装 ErrBackendUnavailable、不进
// FailPolicy 的兜底通道（Backend 代码有 bug 不该被 FailOpen 掩盖）。
var errBrokenBatch = errors.New("ratelimit: wholesale batch aborted (backend panic)")

// wrapUnavailable 包装原始错误为兜底哨兵错误。若 err 已被后端包装为
// ErrBackendUnavailable（errors.Is 命中）则原样返回，避免叠加出
// 「ratelimit: backend unavailable: ratelimit: backend unavailable: …」
// 的冗余前缀（对齐 lock.wrapUnavailable 先例）。
func wrapUnavailable(err error) error {
	if errors.Is(err, ErrBackendUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
}

// failOpenError 是 FailOpen 兜底放行的错误载体：Unwrap 同时返回兜底哨兵
// 与后端原始错误，使 errors.Is(err, ErrFailOpen) 与
// errors.Is(err, ErrBackendUnavailable) 双可判。
type failOpenError struct{ err error }

// Error 实现 error 接口。
func (e *failOpenError) Error() string {
	return fmt.Sprintf("ratelimit: fail-open fallback allowed request: %v", e.err)
}

// Unwrap 返回双链：ErrFailOpen 与后端原始错误。
func (e *failOpenError) Unwrap() []error { return []error{ErrFailOpen, e.err} }

// wrapFailOpen 生成 FailOpen 兜底放行错误（不修饰后端错误原文，仅挂链）。
func wrapFailOpen(err error) error { return &failOpenError{err: err} }
