package logger

import "context"

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
