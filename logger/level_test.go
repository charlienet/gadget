package logger_test

import (
	"testing"

	"github.com/charlienet/gadget/logger"
)

func TestLevelString(t *testing.T) {
	tests := []struct {
		level logger.Level
		want  string
	}{
		{logger.Trace, "TRACE"},
		{logger.Debug, "DEBUG"},
		{logger.Info, "INFO"},
		{logger.Warn, "WARN"},
		{logger.Error, "ERROR"},
		{logger.Fatal, "FATAL"},
	}

	for _, tt := range tests {
		got := tt.level.String()
		if got != tt.want {
			t.Errorf("Level(%d).String() = %q, want %q", int8(tt.level), got, tt.want)
		}
	}
}

func TestLevelEnabled(t *testing.T) {
	tests := []struct {
		current logger.Level
		target  logger.Level
		want    bool
	}{
		// Same level is enabled
		{logger.Info, logger.Info, true},
		{logger.Debug, logger.Debug, true},
		{logger.Fatal, logger.Fatal, true},

		// Higher severity (bigger value) is enabled
		{logger.Debug, logger.Info, true},
		{logger.Debug, logger.Warn, true},
		{logger.Debug, logger.Error, true},
		{logger.Debug, logger.Fatal, true},
		{logger.Info, logger.Warn, true},
		{logger.Info, logger.Error, true},
		{logger.Info, logger.Fatal, true},

		// Lower severity (smaller value) is NOT enabled
		{logger.Info, logger.Debug, false},
		{logger.Info, logger.Trace, false},
		{logger.Warn, logger.Info, false},
		{logger.Warn, logger.Debug, false},
		{logger.Warn, logger.Trace, false},
		{logger.Error, logger.Warn, false},
		{logger.Error, logger.Info, false},
		{logger.Fatal, logger.Error, false},
	}

	for _, tt := range tests {
		got := tt.current.Enabled(tt.target)
		if got != tt.want {
			t.Errorf("Level(%s).Enabled(%s) = %v, want %v",
				tt.current, tt.target, got, tt.want)
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
