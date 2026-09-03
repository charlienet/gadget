// Package retry 提供上下文感知的通用重试执行器。
//
// 三要素模型：一次重试行为由三个正交要素完全决定——
//   - 退避策略（Backoff）：两次尝试之间等待多久。内置 Fixed、
//     Exponential，以及 FullJitter / EqualJitter 两种抖动包装器；
//   - 终止条件（Option）：重试到什么时候停止。MaxAttempts 限制 fn 的
//     总执行次数（含首次，默认 5）；MaxElapsed 限制总耗时（软上限，
//     默认禁用）；两者无优先关系，每轮检查、先到先终止；
//   - 错误分类（WithRetryable）：决定哪些错误值得重试，默认全部
//     错误可重试；判定为不可重试的错误立即原样返回。
//
// 与 gadget/redis 对接：Redis 连接/服务类故障可用 IsUnavailable 精确
// 圈定重试范围，命令级错误（语法错、WRONGTYPE 等）不浪费退避时间：
//
//	err := retry.Do(ctx, func(ctx context.Context) error {
//		return client.Set(ctx, "k", "v", 0).Err()
//	}, retry.WithRetryable(func(err error) bool {
//		return redis.IsUnavailable(err) // 仅连接/服务不可用类错误重试
//	}))
//
// 默认退避为 Exponential(100ms, 2, 30s)，Do 每次调用时新建独立实例，
// 可安全地被多个 goroutine 并发使用。
package retry

import (
	"context"
	"time"
)

// sleepFunc 是退避睡眠的注入点：生产路径只读，语义见 defaultSleep。
// 测试可临时替换以消除真实等待；覆写本变量的测试禁止 t.Parallel()，
// 以免与其他测试互相干扰。
var sleepFunc = defaultSleep

// defaultSleep 等待 d 后返回 nil；期间 ctx 被取消/超时则立即返回
// ctx.Err()。使用 time.Timer + select，确保可被 ctx 中断。
func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		// 零等待也必须给 ctx 检查机会（Do 在睡眠后检查 ctx）。
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Do 按退避策略反复执行 fn，直到成功、终止条件耗尽或 ctx 取消。
//
// 返回语义（四条规则）：
//
//  1. fn 某次返回 nil → 立即返回 nil（成功优先，即使 ctx 已取消）；
//  2. 错误不可重试（WithRetryable 判定 false），或 MaxAttempts /
//     MaxElapsed 耗尽 → 返回最后一次 fn 的原始错误（不包装，
//     errors.Is / errors.As 保真）；
//  3. ctx 在尝试之间或退避睡眠中取消/超时 → 返回 ctx.Err()；
//  4. ctx 在 fn 执行中取消 → 不中断 fn（无法做到），待其返回后
//     按规则 1/2/3 处理。
//
// 其他约定：
//   - 每次调用入口对 Backoff 执行 Reset（传入共享实例时顺序两次 Do
//     的退避序列各自从头开始）；
//   - NextBackOff 返回 ≤0 视为 0：立即重试，但仍给 ctx 检查机会
//     （防御自定义 Backoff 实现）；
//   - MaxElapsed 为软上限，睡眠不被截断：实际耗时可能超出上限至多
//     一个退避间隔；
//   - fn panic 直接穿透，本包不做 recover；
//   - fn 为 nil 时 panic（fail-fast，与 Backoff 构造期校验一致）；
//   - opts 中未指定的项使用默认值：指数退避（100ms 起、倍率 2、
//     封顶 30s，每次 Do 新建实例）、5 次尝试、无时限、全部错误可重试。
func Do(ctx context.Context, fn func(ctx context.Context) error, opts ...Option) error {
	if fn == nil {
		panic("retry: Do 要求 fn 非 nil")
	}
	o := defaultOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}

	b := o.backoff()
	b.Reset()
	start := time.Now()
	var lastErr error

	for attempt := 1; ; attempt++ {
		// 规则 1/4：先执行 fn——ctx 在进入 Do 前已取消时 fn 仍须运行，
		// 成功优先、不可重试错误原样返回均高于 ctx 检查。
		lastErr = fn(ctx)
		if lastErr == nil {
			return nil // 规则 1：成功优先
		}
		if !o.retryable(lastErr) {
			return lastErr // 规则 2：不可重试，原样返回
		}
		if attempt >= o.maxAttempts {
			return lastErr // 规则 2：次数耗尽，返回原始错误
		}

		d := b.NextBackOff()
		if d < 0 {
			d = 0 // 防御：≤0 视为立即重试
		}
		if err := sleepFunc(ctx, d); err != nil {
			return err // 规则 3：睡眠中 ctx 取消/超时
		}

		// 规则 3：尝试之间检查 ctx（睡眠已给等待机会，此处覆盖
		// d=0 或睡眠期间未捕获的取消）。
		if err := ctx.Err(); err != nil {
			return err
		}
		// 规则 2：每轮重试前检查软时限（lastErr 此时必已存在）。
		if o.maxElapsed > 0 && time.Since(start) >= o.maxElapsed {
			return lastErr
		}
	}
}
