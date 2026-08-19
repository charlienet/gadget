package logger

import (
	"context"
	"io"
	"log/slog"
)

type Logger interface {
	WithField(key string, sss any) Logger
	WithFields(field map[string]any) Logger
	// 新增：slog 风格
	With(args ...any) Logger                                   // slog 风格字段（成对 key/value，奇数个时按 slog 规则补 !BADKEY）
	Log(level slog.Level, msg string, args ...any)             // slog 风格记录
	WithAttrs(attrs ...slog.Attr) Logger                       // slog 风格属性（保持 Value Kind）
	LogAttrs(level slog.Level, msg string, attrs ...slog.Attr) // slog 原生 LogAttrs 语义
	WithGroup(name string) Logger                              // slog 分组
	SetLevel(lvl Level)
	SetOutput(out io.Writer)
	Info(args ...any)
	Infof(template string, args ...any)
	Trace(args ...any)
	Tracef(template string, args ...any)
	Debug(args ...any)
	Debugf(template string, args ...any)
	Warn(args ...any)
	Warnf(template string, args ...any)
	Error(args ...any)
	Errorf(template string, args ...any)
	Fatal(args ...any)
	Fatalf(template string, args ...any)
	WithContext(ctx context.Context) context.Context
}

// LogRecorder is a generic logging interface.
type LogRecorder interface {

	// Init initializes options
	Init(options Options)

	// Fields set fields to always be logged
	Fields(fields map[string]any) LogRecorder

	// Log writes a log entry
	Log(level Level, v ...any)

	// Logf writes a formatted log entry
	Logf(level Level, format string, v ...any)

	// String returns the name of logger
	String() string
}
