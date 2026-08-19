package logger

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
)

// Err 将 error 转为 slog.Attr（key 为 "error"）；err 为 nil 时返回空 Attr
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}

	return slog.Any("error", err)
}

// StackTracer 错误携带调用栈（由 Wrap 创建）
type StackTracer interface {
	StackTrace() []string
}

// wrappedError 包装错误并记录创建位置的调用栈
type wrappedError struct {
	err   error
	stack []string
}

func (w *wrappedError) Error() string { return w.err.Error() }

// Unwrap 返回被包装的原始错误（支持 errors.Is/As 链式查找）
func (w *wrappedError) Unwrap() error { return w.err }

// StackTrace 返回创建位置的调用栈（"file:line function" 行切片）
func (w *wrappedError) StackTrace() []string { return w.stack }

// Wrap 包装错误并记录创建位置的调用栈；nil 返回 nil。
// 栈从 Wrap 的调用方开始记录（跳过 runtime.Callers 与 Wrap 自身帧）。
func Wrap(err error) error {
	if err == nil {
		return nil
	}

	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs) // 跳过 runtime.Callers 与 Wrap 自身
	if n == 0 {
		return err
	}

	return &wrappedError{err: err, stack: formatStack(pcs[:n])}
}

// formatStack 将 PC 列表格式化为 "file:line function" 行切片
func formatStack(pcs []uintptr) []string {
	frames := runtime.CallersFrames(pcs)
	stack := make([]string, 0, len(pcs))
	for {
		f, more := frames.Next()
		stack = append(stack, fmt.Sprintf("%s:%d %s", f.File, f.Line, f.Function))
		if !more {
			break
		}
	}

	return stack
}

// stackHandler 错误堆栈装饰器：对实现 StackTracer 的 error 属性自动附加 stack 属性
type stackHandler struct {
	handler slog.Handler
}

// NewStackHandler 包装 handler：对实现 StackTracer 的 error 属性自动附加
// "<key>_stack" 属性（值为栈行拼接，换行分隔）。
func NewStackHandler(handler slog.Handler) slog.Handler {
	return &stackHandler{handler: handler}
}

// Enabled 透传底层 handler 的级别判断（slog 级别数值越小越详细）
func (h *stackHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

// Handle 遍历 attrs，对 KindAny 且实现 StackTracer 的属性追加
// a.Key+"_stack" 属性；无匹配时直接透传原 record。
func (h *stackHandler) Handle(ctx context.Context, r slog.Record) error {
	if !recordHasStack(r) {
		return h.handler.Handle(ctx, r)
	}

	// 重建 record（保留 PC），附加 stack 属性
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(a)
		if st, ok := stackTracerOf(a); ok {
			nr.AddAttrs(slog.String(a.Key+"_stack", strings.Join(st.StackTrace(), "\n")))
		}
		return true
	})

	return h.handler.Handle(ctx, nr)
}

// recordHasStack 判断 record 是否包含实现 StackTracer 的属性
func recordHasStack(r slog.Record) bool {
	has := false
	r.Attrs(func(a slog.Attr) bool {
		if _, ok := stackTracerOf(a); ok {
			has = true
			return false // 提前终止遍历
		}
		return true
	})

	return has
}

// stackTracerOf 若属性值为 KindAny 且实现 StackTracer 则返回其值
func stackTracerOf(a slog.Attr) (StackTracer, bool) {
	if a.Value.Kind() != slog.KindAny {
		return nil, false
	}
	st, ok := a.Value.Any().(StackTracer)
	return st, ok
}

// WithAttrs 与 Handle 逻辑对称：对实现 StackTracer 的预设属性同样追加 "<key>_stack"。
// 否则 l.WithAttrs(logger.Err(err)) 时 error 成为 handler 级预设属性，
// Handle 的 record 遍历不到，堆栈不附加。
func (h *stackHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, a)
		if st, ok := stackTracerOf(a); ok {
			out = append(out, slog.String(a.Key+"_stack", strings.Join(st.StackTrace(), "\n")))
		}
	}

	return &stackHandler{handler: h.handler.WithAttrs(out)}
}

// WithGroup 透传底层 handler
func (h *stackHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	return &stackHandler{handler: h.handler.WithGroup(name)}
}
