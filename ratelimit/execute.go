package ratelimit

import (
	"context"
)

// Execute 是泛型执行器（对齐 breaker.Execute[T] 形态）：先消耗 1 个令牌，
// 放行后执行 fn 并透传其结果。
//
// 固定消耗 n=1；需要一次消耗多枚令牌的场景直接用 Allow。
//
// 行为：
//   - Allow 拒绝（超限/参数错误/ctx 取消/已 Close/FailClosed 兜底）→
//     不调用 fn，返回（零值, err）；
//   - FailOpen 兜底放行（Allow 返回 true + 包装错误）→ 仍执行 fn；
//     fn 自身成功时把兜底错误一并返回（放行可感知），fn 出错时以 fn 错误为准；
//   - fn panic 直接穿透，本包不做 recover。
func Execute[T any](ctx context.Context, l *Limiter, key string, fn func(ctx context.Context) (T, error)) (T, error) {
	ok, allowErr := l.Allow(ctx, key, 1)

	var zero T
	if allowErr != nil && !ok {
		return zero, allowErr
	}

	result, fnErr := fn(ctx)
	if allowErr != nil && fnErr == nil {
		// FailOpen 兜底放行且业务成功：返回业务结果 + 兜底错误。
		return result, allowErr
	}
	return result, fnErr
}
