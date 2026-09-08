package logger_test

import (
	"log/slog"
	"os"
	"reflect"
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
		QueueSize:  10240,  // 与 async.go 引擎默认一致（m-9）
		FileFormat: "json", // 文件输出默认 JSON（向后兼容）
		// 新增字段保持零值（默认行为不变）：NoColor 零值 false → 颜色自动判定；
		// layout 空 → lumberjack 按大小轮换；敏感配置 nil/空 → 不注入对应 Option。
		// Sensitive_Keys 用 nil 而非 []string{}，与 DefaultConfig 未赋值（nil slice）
		// 在 reflect.DeepEqual 下保持一致。
		Layout:         "",
		Sensitive_Keys: nil,
		Sensitive_Mask: "",
	}
	// Config 含 slice 字段（Sensitive_Keys），不可用 ==/!= 直接比较（编译期即报错）；
	// 统一用 reflect.DeepEqual 做逐字段语义比较。
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("DefaultConfig() = %+v, want %+v", cfg, want)
	}
	// 文件输出格式默认必须为 json（新增字段显式确认向后兼容）
	if cfg.FileFormat != "json" {
		t.Errorf("DefaultConfig().FileFormat = %q, want %q", cfg.FileFormat, "json")
	}
	// 控制台颜色默认自动（NoColor 零值 false → 不禁用、走 NO_COLOR+TTY 判定）
	if cfg.NoColor {
		t.Errorf("DefaultConfig().NoColor = %v, want false（默认自动判定）", cfg.NoColor)
	}
	// 零值字段显式确认（Service/Env/File 不设默认）
	if cfg.Service != "" || cfg.Env != "" || cfg.File != "" {
		t.Errorf("expected empty Service/Env/File defaults, got %+v", cfg)
	}
	// 新增字段零值显式确认：layout/sensitive 不设默认（保持既有按大小轮换、无打码行为）。
	// Sensitive_Keys 必须是 nil（非 []string{}），否则与 want 的 DeepEqual 语义不一致。
	if cfg.Layout != "" || cfg.Sensitive_Keys != nil || cfg.Sensitive_Mask != "" {
		t.Errorf("expected empty layout/sensitive defaults, got layout=%q keys=%v mask=%q",
			cfg.Layout, cfg.Sensitive_Keys, cfg.Sensitive_Mask)
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
	logger.WithFormat(logger.FormatText)(fo)
	if fo.Format != logger.FormatText {
		t.Errorf("WithFormat: got %q, want text", fo.Format)
	}
}

// Options 级 Option 构造器断言（直接作用于 Options 结构，不建 logger）
func TestOptionBuilders(t *testing.T) {
	var o logger.Options

	logger.WithLevel(logger.Warn)(&o)
	if o.Level != logger.Warn {
		t.Errorf("WithLevel: got %v", o.Level)
	}
	logger.WithConsole(logger.WithConsoleWriter(os.Stdout))(&o)
	if o.Console == nil || o.Console.Writer != os.Stdout {
		t.Error("WithConsole(WithConsoleWriter): want os.Stdout")
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
	logger.WithConsole(logger.WithConsoleColor(false))(&o)
	if o.Console == nil || o.Console.Color == nil || *o.Console.Color {
		t.Error("WithConsole(WithConsoleColor(false)): want explicit false")
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

	// WithFormat：文件输出格式透传（并入 WithFile 子选项，FileFormat 枚举类型）
	logger.WithFile("/tmp/y.log", logger.WithFormat(logger.FormatText))(&o)
	if o.FileOpts == nil || o.FileOpts.Format != logger.FormatText {
		t.Errorf("WithFormat: got %+v, want text", o.FileOpts)
	}
}

// ParseFileFormat：yaml 字符串 → FileFormat 枚举。空→JSON、json/text（大小写不敏感+trim）→对应、
// 其它非空→error（不静默回退）。同时验证 FileFormat.String()。
func TestParseFileFormat(t *testing.T) {
	ok := []struct {
		in   string
		want logger.FileFormat
	}{
		{"", logger.FormatJSON},
		{"json", logger.FormatJSON},
		{"JSON", logger.FormatJSON},
		{"  Json  ", logger.FormatJSON},
		{"text", logger.FormatText},
		{"TEXT", logger.FormatText},
		{" text ", logger.FormatText},
	}
	for _, c := range ok {
		got, err := logger.ParseFileFormat(c.in)
		if err != nil {
			t.Errorf("ParseFileFormat(%q): unexpected err %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseFileFormat(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// 未知非空值 → error（不静默回退）
	if _, err := logger.ParseFileFormat("yaml"); err == nil {
		t.Error(`ParseFileFormat("yaml"): want error for unknown value`)
	}

	// String()
	if logger.FormatText.String() != "text" || logger.FormatJSON.String() != "json" {
		t.Error("FileFormat.String() mismatch")
	}
}
