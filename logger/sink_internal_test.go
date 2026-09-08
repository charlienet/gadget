package logger

import (
	"io"
	"log/slog"
	"testing"
)

// TestShouldEnableConsole：sink 存在性决策真值表——
// 零 sink 兜底 stdout console；纯文件关闭 console；both / 纯 console 启用。
func TestShouldEnableConsole(t *testing.T) {
	cases := []struct {
		hasConsole, hasFile bool
		want                bool
		desc                string
	}{
		{false, false, true, "零 sink 兜底"},
		{false, true, false, "纯文件不写 console"},
		{true, true, true, "both 双端"},
		{true, false, true, "显式 console"},
	}
	for _, c := range cases {
		if got := shouldEnableConsole(c.hasConsole, c.hasFile); got != c.want {
			t.Errorf("shouldEnableConsole(%v,%v)=%v want %v (%s)",
				c.hasConsole, c.hasFile, got, c.want, c.desc)
		}
	}
}

// TestBuildOptionsSinkSemantics：buildOptions 基线、sink 存在性、
// 同名 Option 后者胜、WithConsole 多次叠加到同一 ConsoleSettings、WithFile+WithFormat。
func TestBuildOptionsSinkSemantics(t *testing.T) {
	// 基线：Level=Info，无任何 sink
	base := buildOptions()
	if base.Level != slog.LevelInfo {
		t.Errorf("baseline Level=%v want Info", base.Level)
	}
	if base.Console != nil || base.File != "" {
		t.Error("baseline must declare no sink (Console nil, File empty)")
	}

	// 同名 Option 后者胜
	if got := buildOptions(WithLevel(slog.LevelWarn), WithLevel(slog.LevelError)); got.Level != slog.LevelError {
		t.Errorf("last-wins Level=%v want Error", got.Level)
	}

	// WithConsole() 即便无子选项也置 Console 非 nil（表达「启用控制台 sink」）
	if got := buildOptions(WithConsole()); got.Console == nil {
		t.Error("WithConsole() must set Console non-nil")
	}

	// WithConsole 多次调用叠加到同一 ConsoleSettings（Writer + Color 都保留）
	merged := buildOptions(
		WithConsole(WithConsoleWriter(io.Discard)),
		WithConsole(WithConsoleColor(false)),
	)
	if merged.Console == nil || merged.Console.Writer != io.Discard ||
		merged.Console.Color == nil || *merged.Console.Color {
		t.Errorf("WithConsole stacking broken: %+v", merged.Console)
	}

	// WithFile 只启用文件 sink，不连带启用 console
	fileOnly := buildOptions(WithFile("/tmp/z.log", WithFormat(FormatText)))
	if fileOnly.Console != nil {
		t.Error("WithFile must not enable console sink")
	}
	if fileOnly.File != "/tmp/z.log" || fileOnly.FileOpts == nil || fileOnly.FileOpts.Format != FormatText {
		t.Errorf("WithFile sink/format wrong: File=%q FileOpts=%+v", fileOnly.File, fileOnly.FileOpts)
	}
}
