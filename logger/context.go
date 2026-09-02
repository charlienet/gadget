package logger

import (
	"context"
	"log/slog"
)

// trace 上下文工具：trace_id / req_id 的注入与提取。
// ctx key 使用空 struct 类型（编译期隔离，禁止 string key 引发跨包冲突）。

// TraceHandler 注入的日志属性名
const (
	AttrTraceID = "trace_id"
	AttrReqID   = "req_id"
)

type traceIDKey struct{}

type reqIDKey struct{}

// WithTraceID 将 trace_id 注入 context
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

// GetTraceID 从 context 提取 trace_id（未注入或 ctx 为 nil 时返回空串）
func GetTraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(traceIDKey{}).(string); ok {
		return v
	}

	return ""
}

// WithReqID 将 req_id 注入 context
func WithReqID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey{}, id)
}

// GetReqID 从 context 提取 req_id（未注入或 ctx 为 nil 时返回空串）
func GetReqID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(reqIDKey{}).(string); ok {
		return v
	}

	return ""
}

// TraceHandler 内置 trace 注入装饰器：Handle 时从 ctx 提取非空 trace_id/req_id
// 注入为日志属性。New 组装的 handler 链总是包裹本装饰器（默认行为，无开关），
// 因此所有 *Context 方法（InfoContext 等）自动带上链路标识；
// 非 Context 方法（slog 便捷方法 ctx 为 nil）直接透传，不产生属性。
type TraceHandler struct{ slog.Handler }

// NewTraceHandler 包装内层 handler
func NewTraceHandler(h slog.Handler) *TraceHandler {
	return &TraceHandler{Handler: h}
}

// Handle 提取 trace_id/req_id 追加到 record 后透传内层；ctx 为 nil 时直接透传。
// 属性名用编译期常量，避免与用户 attrs 撞名时的顺序歧义（AddAttrs 追加在末尾）。
func (h *TraceHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx == nil {
		return h.Handler.Handle(ctx, r)
	}

	if id := GetTraceID(ctx); id != "" {
		r.AddAttrs(slog.String(AttrTraceID, id))
	}
	if id := GetReqID(ctx); id != "" {
		r.AddAttrs(slog.String(AttrReqID, id))
	}

	return h.Handler.Handle(ctx, r)
}

// WithAttrs 代理内层 handler 并保持 TraceHandler 装饰（派生 logger 的 *Context 方法
// 仍自动注入 trace_id/req_id；纯透传会使 With 派生后装饰器丢失）
func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup 代理内层 handler 并保持 TraceHandler 装饰（同 WithAttrs）
func (h *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{Handler: h.Handler.WithGroup(name)}
}
