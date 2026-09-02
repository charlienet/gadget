package logger_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charlienet/gadget/logger"
)

// newBufLogger 创建输出到 buffer 的 logger（关闭颜色，级别显式指定）
func newBufLogger(t *testing.T, level logger.Level) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l := logger.New(logger.WithLevel(level), logger.WithOutput(&buf), logger.WithColor(false))
	return l, &buf
}

func TestInfo(t *testing.T) {
	l, buf := newBufLogger(t, logger.Info)
	l.Info("hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("expected Info output, got: %s", buf.String())
	}
}

func TestLevelFiltering(t *testing.T) {
	l, buf := newBufLogger(t, logger.Warn)

	l.Debug("hidden")
	if buf.Len() > 0 {
		t.Errorf("expected no Debug output at Warn level, got: %s", buf.String())
	}

	l.Info("hidden")
	if buf.Len() > 0 {
		t.Errorf("expected no Info output at Warn level, got: %s", buf.String())
	}

	l.Warn("visible")
	if buf.Len() == 0 {
		t.Fatal("expected Warn output at Warn level")
	}
}

func TestWithAttr(t *testing.T) {
	l, buf := newBufLogger(t, logger.Debug)
	l.With("key", "val").Debug("msg")
	got := buf.String()
	if !strings.Contains(got, "key=val") {
		t.Errorf("expected field in output, got: %s", got)
	}
}

func TestWithMultipleAttrOrder(t *testing.T) {
	// 原生 slog With 按调用方给定的成对顺序输出（原 WithFields map 排序能力的等价物）
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false))
	l.With("a", 2, "m", 3, "z", 1).Info("msg")

	got := buf.String()
	ai := strings.Index(got, "a=2")
	mi := strings.Index(got, "m=3")
	zi := strings.Index(got, "z=1")
	if ai < 0 || mi < 0 || zi < 0 {
		t.Fatalf("expected all fields in output, got: %s", got)
	}
	if !(ai < mi && mi < zi) {
		t.Errorf("expected caller order a<m<z, got: %s", got)
	}
}

func TestSlogWithLog(t *testing.T) {
	l, buf := newBufLogger(t, logger.Info)

	l.With("key", "val").Log(context.Background(), logger.Info, "slog msg")
	got := buf.String()
	if !strings.Contains(got, "slog msg") {
		t.Errorf("expected message in output, got: %s", got)
	}
	if !strings.Contains(got, "key=val") {
		t.Errorf("expected key=val in output, got: %s", got)
	}
}

func TestWithAttrKind(t *testing.T) {
	l, buf := newBufLogger(t, logger.Info)

	// slog.Attr 直接作为 With 参数：保持 Value Kind（等价 WithAttrs 语义；
	// 本工具链的 slog.Logger 无 WithAttrs 方法）
	l.With(slog.String("k", "v")).Info("msg")
	got := buf.String()
	if !strings.Contains(got, "k=v") {
		t.Errorf("expected k=v in output, got: %s", got)
	}
	if !strings.Contains(got, "msg") {
		t.Errorf("expected msg in output, got: %s", got)
	}
}

func TestLogAttrs(t *testing.T) {
	l, buf := newBufLogger(t, logger.Info)

	l.LogAttrs(context.Background(), logger.Info, "msg", slog.Int("n", 1))
	got := buf.String()
	if !strings.Contains(got, "msg") {
		t.Errorf("expected msg in output, got: %s", got)
	}
	if !strings.Contains(got, "n=1") {
		t.Errorf("expected n=1 in output, got: %s", got)
	}
}

func TestWithGroup(t *testing.T) {
	l, buf := newBufLogger(t, logger.Info)

	l.WithGroup("g").With(slog.String("k", "v")).Info("msg")
	if !strings.Contains(buf.String(), "g.k=v") {
		t.Errorf("expected g.k=v in output, got: %s", buf.String())
	}
}

func TestTraceLevel(t *testing.T) {
	// Trace 级别经 slog 原生 Log 输出 [TRAC]
	l, buf := newBufLogger(t, logger.Trace)
	l.Log(context.Background(), logger.Trace, "trace msg")
	if !strings.Contains(buf.String(), "[TRAC] trace msg") {
		t.Errorf("expected trace output, got: %s", buf.String())
	}
}

func TestDefaultLogger(t *testing.T) {
	// Should not panic
	l := logger.DefaultLogger
	if l == nil {
		t.Fatal("expected non-nil DefaultLogger")
	}
	l.Info("default ok")
}

func TestNewSlogDefault(t *testing.T) {
	// 无参 New() 不 panic，返回可用 *slog.Logger
	l := logger.New()
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
	l.Info("no panic")

	// WithOutput 后输出 Info 消息
	var buf bytes.Buffer
	l2 := logger.New(logger.WithOutput(&buf), logger.WithColor(false))
	l2.Info("default slog works")
	if !strings.Contains(buf.String(), "default slog works") {
		t.Errorf("expected message in output, got: %s", buf.String())
	}
}

// SetDefault 生效：New 返回的 logger 即 slog.Default()，包级函数走同一条 handler 链
func TestSetDefaultApplied(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false))

	if slog.Default() != l {
		t.Error("expected New to slog.SetDefault the returned logger")
	}
	slog.Info("via package fn")
	if !strings.Contains(buf.String(), "via package fn") {
		t.Errorf("expected package-level slog.Info routed to New logger, got: %s", buf.String())
	}
}

// WithService/WithEnv 注入 service/env 属性
func TestServiceEnvInjection(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false),
		logger.WithService("pay-svc"), logger.WithEnv("prod"))

	l.Info("msg")
	got := buf.String()
	if !strings.Contains(got, "service=pay-svc") {
		t.Errorf("expected service attr, got: %s", got)
	}
	if !strings.Contains(got, "env=prod") {
		t.Errorf("expected env attr, got: %s", got)
	}

	// 未设置时不出现对应属性
	var buf2 bytes.Buffer
	l2 := logger.New(logger.WithOutput(&buf2), logger.WithColor(false))
	l2.Info("msg")
	if strings.Contains(buf2.String(), "service=") || strings.Contains(buf2.String(), "env=") {
		t.Errorf("expected no service/env attrs when not configured, got: %s", buf2.String())
	}
}

// 包级 SetLevel 动态调整最近一次 New 实例的级别
func TestSetLevelPackage(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithLevel(logger.Error), logger.WithOutput(&buf), logger.WithColor(false))

	l.Debug("hidden")
	if buf.Len() > 0 {
		t.Fatalf("expected no output at Error level, got: %s", buf.String())
	}

	logger.SetLevel(logger.Debug)
	defer logger.SetLevel(logger.Info) // 复位，避免影响其他测试的默认级别直觉
	l.Debug("now visible")
	if !strings.Contains(buf.String(), "now visible") {
		t.Errorf("expected Debug output after SetLevel(Debug), got: %s", buf.String())
	}
}

// WithLeveler 自定义 Leveler 优先于 WithLevel 静态级别，运行时可动态调整
func TestLevelerDynamic(t *testing.T) {
	var buf bytes.Buffer
	dl := logger.NewDynamicLevel(logger.Error)
	l := logger.New(logger.WithLeveler(dl), logger.WithLevel(logger.Info), logger.WithOutput(&buf), logger.WithColor(false))

	l.Info("hidden") // Leveler=Error，Info 应被过滤
	if buf.Len() > 0 {
		t.Fatalf("expected Leveler to override WithLevel, got: %s", buf.String())
	}

	dl.Set(logger.Debug)
	l.Debug("now visible")
	if !strings.Contains(buf.String(), "now visible") {
		t.Errorf("expected Debug output after leveler.Set(Debug), got: %s", buf.String())
	}
}

// TraceHandler：*Context 方法自动注入 trace_id/req_id；非 Context 方法不注入
func TestTraceContextInjection(t *testing.T) {
	l, buf := newBufLogger(t, logger.Info)

	ctx := logger.WithTraceID(context.Background(), "t-1")
	ctx = logger.WithReqID(ctx, "r-1")

	l.InfoContext(ctx, "with trace")
	got := buf.String()
	if !strings.Contains(got, "trace_id=t-1") {
		t.Errorf("expected trace_id in output, got: %s", got)
	}
	if !strings.Contains(got, "req_id=r-1") {
		t.Errorf("expected req_id in output, got: %s", got)
	}

	// 未注入 trace 的 ctx：不出现属性
	buf.Reset()
	l.InfoContext(context.Background(), "no trace")
	if strings.Contains(buf.String(), "trace_id") || strings.Contains(buf.String(), "req_id") {
		t.Errorf("expected no trace attrs without ctx injection, got: %s", buf.String())
	}

	// 便捷方法（ctx 为 nil）：直接透传，不 panic、无属性
	buf.Reset()
	l.Info("convenience")
	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("expected no trace attrs for convenience method, got: %s", buf.String())
	}
}

// TraceHandler 对 With/WithGroup 派生实例仍生效（派生后装饰器不丢失）
func TestTraceOnDerivedLogger(t *testing.T) {
	l, buf := newBufLogger(t, logger.Info)

	ctx := logger.WithTraceID(context.Background(), "t-2")
	l.With("k", "v").InfoContext(ctx, "derived")
	if !strings.Contains(buf.String(), "trace_id=t-2") || !strings.Contains(buf.String(), "k=v") {
		t.Errorf("expected trace_id on With-derived logger, got: %s", buf.String())
	}

	buf.Reset()
	l.WithGroup("g").InfoContext(ctx, "grouped")
	if !strings.Contains(buf.String(), "trace_id=t-2") {
		t.Errorf("expected trace_id on WithGroup-derived logger, got: %s", buf.String())
	}
}

// trace ctx 工具函数的存取语义
func TestTraceCtxHelpers(t *testing.T) {
	if got := logger.GetTraceID(context.Background()); got != "" {
		t.Errorf("expected empty trace id, got %q", got)
	}
	if got := logger.GetReqID(context.Background()); got != "" {
		t.Errorf("expected empty req id, got %q", got)
	}

	ctx := logger.WithTraceID(logger.WithReqID(context.Background(), "r9"), "t9")
	if logger.GetTraceID(ctx) != "t9" || logger.GetReqID(ctx) != "r9" {
		t.Error("expected trace/req id round-trip")
	}
}

// 包级 Fatal/Fatalf：ExitFunc 注入替换，Fatal 级别记录 + 退出码 1
func TestFatalPackageLevel(t *testing.T) {
	var exitCode int
	origExit := logger.ExitFunc
	logger.ExitFunc = func(code int) { exitCode = code }
	defer func() { logger.ExitFunc = origExit }()

	origDefault := logger.DefaultLogger
	defer func() { logger.DefaultLogger = origDefault }()

	l, buf := newBufLogger(t, logger.Info)
	logger.DefaultLogger = l

	logger.Fatal("exit test", "k", "v")
	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
	got := buf.String()
	if !strings.Contains(got, "[FATA] exit test") {
		t.Errorf("expected fatal message with FATA level, got: %s", got)
	}
	if !strings.Contains(got, "k=v") {
		t.Errorf("expected attrs in fatal output, got: %s", got)
	}

	// Fatalf：格式化消息
	exitCode = 0
	buf.Reset()
	logger.Fatalf("fmt %s %d", "abc", 42)
	if exitCode != 1 {
		t.Errorf("expected exit code 1 for Fatalf, got %d", exitCode)
	}
	if !strings.Contains(buf.String(), "fmt abc 42") {
		t.Errorf("expected formatted output, got: %s", buf.String())
	}
}

func TestConsoleColor(t *testing.T) {
	// 确保不受环境变量影响
	t.Setenv("NO_COLOR", "")

	// WithColor(true)：非 TTY（bytes.Buffer）也强制输出 ANSI 转义序列
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(true))
	l.Info("colored")
	if !strings.Contains(buf.String(), "\033[") {
		t.Errorf("expected ANSI color codes, got: %q", buf.String())
	}

	// WithColor(false)：无 ANSI 转义
	var buf2 bytes.Buffer
	l2 := logger.New(logger.WithOutput(&buf2), logger.WithColor(false))
	l2.Info("plain")
	if strings.Contains(buf2.String(), "\033[") {
		t.Errorf("expected no ANSI color codes, got: %q", buf2.String())
	}
}

func TestFileOutput(t *testing.T) {
	// 手动创建临时目录：lumberjack 不主动释放文件句柄，
	// 若用 t.TempDir 自动清理会因句柄占用而失败。
	dir, err := os.MkdirTemp("", "logger-file-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(dir) // 句柄占用时删除会失败，进程退出后自动释放，忽略错误

	path := filepath.Join(dir, "app.log")

	l := logger.New(logger.WithFile(path))
	l.Info("file message")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if !strings.Contains(string(data), "file message") {
		t.Errorf("expected message in file, got: %s", string(data))
	}
}

// 文件 + trace ctx：JSON 文件输出同样带 trace_id
func TestFileOutputWithTrace(t *testing.T) {
	dir, err := os.MkdirTemp("", "logger-file-trace-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "app.log")
	l := logger.New(logger.WithOutput(os.Stdout), logger.WithFile(path))

	ctx := logger.WithTraceID(context.Background(), "file-trace-1")
	l.InfoContext(ctx, "traced file msg")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if !strings.Contains(string(data), `"trace_id":"file-trace-1"`) {
		t.Errorf("expected trace_id in file JSON, got: %s", string(data))
	}
}

// --- 并发安全 ---

func TestConcurrentSafety(t *testing.T) {
	l := logger.New(logger.WithLevel(logger.Debug), logger.WithOutput(blackHole{}), logger.WithColor(false))

	var wg sync.WaitGroup

	// Multiple goroutines logging concurrently
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				l.Info("concurrent info")
				l.Debug("concurrent debug")
				l.With("key", "val").Warn("with attr")
			}
		}()
	}

	// 包级 SetLevel 与日志并发（DynamicLevel 原子级别）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			logger.SetLevel(logger.Warn)
			logger.SetLevel(logger.Debug)
		}
	}()

	// With 派生与日志并发
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				l.With("k", "v").Info("derived")
			}
		}()
	}

	wg.Wait()
}

// blackHole implements io.Writer discarding all data (thread-safe).
type blackHole struct{}

func (blackHole) Write(p []byte) (int, error) { return len(p), nil }
