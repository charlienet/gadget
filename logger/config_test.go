package logger_test

import (
	"log/slog"
	"os"
	"testing"

	"github.com/charlienet/gadget/logger"
)

// --- ① 配置驱动路径 ---

func TestDefaultConfig(t *testing.T) {
	cfg := logger.DefaultConfig()

	want := logger.Config{
		Level:      "info",
		Output:     "console",
		MaxSize:    100,
		MaxAge:     30,
		MaxBackups: 10,
		Compress:   true,
		Async:      false,
		QueueSize:  10240, // 与 async.go 引擎默认一致（m-9）
	}
	if cfg != want {
		t.Errorf("DefaultConfig() = %+v, want %+v", cfg, want)
	}
	// 零值字段显式确认（Service/Env/File 不设默认）
	if cfg.Service != "" || cfg.Env != "" || cfg.File != "" {
		t.Errorf("expected empty Service/Env/File defaults, got %+v", cfg)
	}
}

func TestParseLevelTable(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"trace", logger.Trace},
		{"TRACE", logger.Trace}, // 大小写不敏感
		{"debug", logger.Debug},
		{"info", logger.Info},
		{"warn", logger.Warn},
		{"error", logger.Error},
		{"fatal", logger.FatalLevel},
		{"", logger.Info},      // 空串回退 Info
		{"bogus", logger.Info}, // 未知回退 Info
		{"InFo", logger.Info},  // 混合大小写
	}

	for _, tt := range tests {
		if got := logger.ParseLevel(tt.in); got != tt.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestLevelFromEnv(t *testing.T) {
	// LOG_LEVEL 生效
	t.Setenv("LOG_LEVEL", "warn")
	if got := logger.LevelFromEnv(); got != logger.Warn {
		t.Errorf("LevelFromEnv() with LOG_LEVEL=warn = %v, want WARN", got)
	}

	// 空值/未设置回退 Info
	t.Setenv("LOG_LEVEL", "")
	if got := logger.LevelFromEnv(); got != logger.Info {
		t.Errorf("LevelFromEnv() with LOG_LEVEL empty = %v, want INFO", got)
	}

	// 非法值回退 Info
	t.Setenv("LOG_LEVEL", "not-a-level")
	if got := logger.LevelFromEnv(); got != logger.Info {
		t.Errorf("LevelFromEnv() with LOG_LEVEL bogus = %v, want INFO", got)
	}
}

// FileOption 构造器逐一断言字段写入
func TestFileOptionBuilders(t *testing.T) {
	fo := &logger.FileOptions{}

	logger.WithMaxSize(64)(fo)
	if fo.MaxSize != 64 {
		t.Errorf("WithMaxSize: got %d, want 64", fo.MaxSize)
	}
	logger.WithMaxAge(7)(fo)
	if fo.MaxAge != 7 {
		t.Errorf("WithMaxAge: got %d, want 7", fo.MaxAge)
	}
	logger.WithMaxBackups(3)(fo)
	if fo.MaxBackups != 3 {
		t.Errorf("WithMaxBackups: got %d, want 3", fo.MaxBackups)
	}
	logger.WithCompress(true)(fo)
	if !fo.Compress {
		t.Error("WithCompress: want true")
	}
	logger.WithDateRotate("2006-01")(fo)
	if fo.Layout != "2006-01" {
		t.Errorf("WithDateRotate: got %q, want 2006-01", fo.Layout)
	}
}

// Options 级 Option 构造器断言（直接作用于 Options 结构，不建 logger）
func TestOptionBuilders(t *testing.T) {
	var o logger.Options

	logger.WithLevel(logger.Warn)(&o)
	if o.Level != logger.Warn {
		t.Errorf("WithLevel: got %v", o.Level)
	}
	logger.WithOutput(os.Stdout)(&o)
	if o.Out != os.Stdout {
		t.Error("WithOutput: want os.Stdout")
	}
	logger.WithService("svc")(&o)
	if o.Service != "svc" {
		t.Errorf("WithService: got %q", o.Service)
	}
	logger.WithEnv("prod")(&o)
	if o.Env != "prod" {
		t.Errorf("WithEnv: got %q", o.Env)
	}
	dl := logger.NewDynamicLevel(logger.Debug)
	logger.WithLeveler(dl)(&o)
	if o.Leveler != dl {
		t.Error("WithLeveler: want the provided leveler")
	}
	logger.WithSource(true)(&o)
	if !o.Source {
		t.Error("WithSource: want true")
	}
	logger.WithColor(false)(&o)
	if o.Color == nil || *o.Color {
		t.Error("WithColor(false): want explicit false")
	}
	logger.WithAsync(128)(&o)
	if !o.Async || o.QueueSize != 128 {
		t.Errorf("WithAsync: got Async=%v QueueSize=%d", o.Async, o.QueueSize)
	}
	logger.WithAsync()(&o) // 省略容量不改写
	if o.QueueSize != 128 {
		t.Errorf("WithAsync() without size should keep 128, got %d", o.QueueSize)
	}
	logger.WithAsyncBlocking()(&o)
	if !o.AsyncBlocking {
		t.Error("WithAsyncBlocking: want true")
	}
	logger.WithStackTrace(true)(&o)
	if !o.StackTrace {
		t.Error("WithStackTrace: want true")
	}
	logger.WithSampling(5, 10)(&o)
	if o.Sampling == nil || o.Sampling.First != 5 || o.Sampling.Thereafter != 10 {
		t.Errorf("WithSampling: got %+v", o.Sampling)
	}
	logger.WithSensitiveKeys("phone")(&o)
	if o.Sensitive == nil || len(o.Sensitive.Keys) != 1 || o.Sensitive.Keys[0] != "phone" {
		t.Errorf("WithSensitiveKeys: got %+v", o.Sensitive)
	}
	logger.WithSensitiveMask("[M]")(&o)
	if o.Sensitive.Mask != "[M]" {
		t.Errorf("WithSensitiveMask: got %q", o.Sensitive.Mask)
	}
	logger.WithSensitiveMatch(func(string) bool { return true })(&o)
	if o.Sensitive.Match == nil {
		t.Error("WithSensitiveMatch: want non-nil")
	}

	// WithFile：路径 + FileOptions 透传
	logger.WithFile("/tmp/x.log", logger.WithMaxSize(1), logger.WithDateRotate("2006"))(&o)
	if o.File != "/tmp/x.log" || o.FileOpts == nil || o.FileOpts.MaxSize != 1 || o.FileOpts.Layout != "2006" {
		t.Errorf("WithFile: got File=%q FileOpts=%+v", o.File, o.FileOpts)
	}
}
