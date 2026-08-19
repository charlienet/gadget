package logger

import (
	"log/slog"
	"sync/atomic"
)

// DynamicLevel 动态日志级别，实现 slog.Leveler，运行时可调
type DynamicLevel struct{ level atomic.Int32 }

// NewDynamicLevel 创建动态级别
func NewDynamicLevel(level slog.Level) *DynamicLevel {
	l := &DynamicLevel{}
	l.level.Store(int32(level))
	return l
}

// Level 返回当前级别（slog.Leveler 接口）
func (l *DynamicLevel) Level() slog.Level {
	return slog.Level(l.level.Load())
}

// Set 运行时调整级别
func (l *DynamicLevel) Set(level slog.Level) {
	l.level.Store(int32(level))
}
