package logger_test

import (
	"testing"

	"github.com/charlienet/gadget/logger"
)

func TestLevelString(t *testing.T) {
	// Level 是 slog.Level 别名，String() 遵循 slog 规则：
	// 命名级别返回大写名（DEBUG/INFO/WARN/ERROR），自定义值返回 base+偏移
	tests := []struct {
		level logger.Level
		want  string
	}{
		{logger.Trace, "DEBUG-4"},
		{logger.Debug, "DEBUG"},
		{logger.Info, "INFO"},
		{logger.Warn, "WARN"},
		{logger.Error, "ERROR"},
		{logger.Fatal, "ERROR+4"},
	}

	for _, tt := range tests {
		got := tt.level.String()
		if got != tt.want {
			t.Errorf("Level(%d).String() = %q, want %q", int(tt.level), got, tt.want)
		}
	}
}

func TestGetLevel(t *testing.T) {
	tests := []struct {
		input string
		want  logger.Level
	}{
		{"TRACE", logger.Trace},
		{"DEBUG", logger.Debug},
		{"INFO", logger.Info},
		{"WARN", logger.Warn},
		{"ERROR", logger.Error},
		{"FATAL", logger.Fatal},
		// case insensitive
		{"trace", logger.Trace},
		{"Trace", logger.Trace},
		{"info", logger.Info},
	}

	for _, tt := range tests {
		got, err := logger.GetLevel(tt.input)
		if err != nil {
			t.Errorf("GetLevel(%q) unexpected error: %v", tt.input, err)
		}
		if got != tt.want {
			t.Errorf("GetLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestGetLevelUnknown(t *testing.T) {
	got, err := logger.GetLevel("UNKNOWN")
	if err == nil {
		t.Error("expected error for unknown level")
	}
	if got != logger.Info {
		t.Errorf("expected Info fallback, got %v", got)
	}
}
