package cache

import (
	"fmt"
	"log/slog"
	"os"
)

// Logger 定义缓存包内部的日志抽象，解除对 gadget/logger 模块的依赖。
// 仅保留 Warn 及以上级别（降级状态与失败告警为强必要路径）；
// Debug/Info 级已从接口移除，应用端注入的日志器无需关心调试级输出。
type Logger interface {
	Warn(args ...any)
	Warnf(format string, args ...any)
	Error(args ...any)
	Errorf(format string, args ...any)
}

// defaultSlog 是基于标准库 log/slog 的默认日志器，输出到 os.Stderr，
// 级别为 Warn（低于 Warn 的日志被过滤，不产生任何输出）。
var defaultSlog = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
	Level: slog.LevelWarn,
}))

// DefaultLogger 是默认的日志实现，不引入任何第三方依赖。
var DefaultLogger Logger = slogLogger{}

type slogLogger struct{}

func (slogLogger) Warn(args ...any)                  { defaultSlog.Warn(fmt.Sprint(args...)) }
func (slogLogger) Warnf(format string, args ...any)  { defaultSlog.Warn(fmt.Sprintf(format, args...)) }
func (slogLogger) Error(args ...any)                 { defaultSlog.Error(fmt.Sprint(args...)) }
func (slogLogger) Errorf(format string, args ...any) { defaultSlog.Error(fmt.Sprintf(format, args...)) }
