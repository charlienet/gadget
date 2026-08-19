package logger

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Level 直接复用 slog 级别体系（现代化：与 slog 完全一致）
type Level = slog.Level

const (
	Trace Level = slog.Level(-8)
	Debug Level = slog.LevelDebug // -4
	Info  Level = slog.LevelInfo  // 0
	Warn  Level = slog.LevelWarn  // 4
	Error Level = slog.LevelError // 8
	Fatal Level = slog.Level(12)
)

// GetLevel 解析级别字符串（保留原有 API，返回类型不变）
func GetLevel(levelStr string) (Level, error) {
	switch strings.ToUpper(levelStr) {
	case "TRACE":
		return Trace, nil
	case "DEBUG":
		return Debug, nil
	case "INFO":
		return Info, nil
	case "WARN":
		return Warn, nil
	case "ERROR":
		return Error, nil
	case "FATAL":
		return Fatal, nil
	}

	return Info, fmt.Errorf("unknown Level String: '%s', defaulting to InfoLevel", levelStr)
}

// ParseLevel 解析日志级别字符串（trace/debug/info/warn/error/fatal，大小写不敏感）
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "trace":
		return Trace
	case "debug":
		return Debug
	case "info":
		return Info
	case "warn":
		return Warn
	case "error":
		return Error
	case "fatal":
		return Fatal
	default:
		return Info
	}
}

// LevelFromEnv 从环境变量 LOG_LEVEL 读取日志级别（未设置时默认 Info）
func LevelFromEnv() slog.Level {
	env := os.Getenv("LOG_LEVEL")
	if env == "" {
		return Info
	}
	return ParseLevel(env)
}
