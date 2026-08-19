package logger

import (
	"context"
)

type contextKey struct{}

var loggerContextKey = &contextKey{}

type LoggerContext struct {
	logger Logger
}

func WithLogger(parent context.Context, l Logger) context.Context {
	return context.WithValue(parent, loggerContextKey, &LoggerContext{
		logger: l,
	})
}

// WithContext 将 logger 注入 context（WithLogger 的别名，命名对齐 slog 生态习惯）
func WithContext(ctx context.Context, l Logger) context.Context {
	return WithLogger(ctx, l)
}

func FromContext(ctx context.Context) Logger {
	if ctx == nil {
		return DefaultLogger
	}

	val := ctx.Value(loggerContextKey)
	if val == nil {
		return DefaultLogger
	}

	if lc, ok := val.(*LoggerContext); ok {
		return lc.logger
	}

	return DefaultLogger
}

// ObtainLogger 从 context 获取请求级 logger（FromContext 的别名，命名对齐 aide 生态习惯）。
// 如果 ctx 为 nil 或未注入 logger，返回包级默认 logger。
func ObtainLogger(ctx context.Context) Logger {
	return FromContext(ctx)
}
