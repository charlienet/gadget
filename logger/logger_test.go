package logger_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charlienet/gadget/logger"
)

func captureLogger(t *testing.T, level logger.Level) (logger.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l := logger.New(logger.WithRecorder(&mockRecorder{}), logger.WithLevel(level), logger.WithOutput(&buf))
	return l, &buf
}

func TestInfo(t *testing.T) {
	l, buf := captureLogger(t, logger.Info)
	l.Info("hello")
	if buf.Len() == 0 {
		t.Error("expected Info output")
	}
}

func TestLevelFiltering(t *testing.T) {
	l, buf := captureLogger(t, logger.Warn)

	l.Debug("hidden")
	if buf.Len() > 0 {
		t.Errorf("expected no Debug output at Warn level, got: %s", buf.String())
	}
	buf.Reset()

	l.Info("hidden")
	if buf.Len() > 0 {
		t.Errorf("expected no Info output at Warn level, got: %s", buf.String())
	}
	buf.Reset()

	l.Warn("visible")
	if buf.Len() == 0 {
		t.Fatal("expected Warn output at Warn level")
	}
}

func TestLogf(t *testing.T) {
	l, buf := captureLogger(t, logger.Info)
	l.Infof("fmt %s %d", "abc", 42)
	if !strings.Contains(buf.String(), "fmt abc 42") {
		t.Errorf("expected formatted output, got: %s", buf.String())
	}
}

func TestWithField(t *testing.T) {
	l, buf := captureLogger(t, logger.Debug)
	l.WithField("key", "val").Debug("msg")
	got := buf.String()
	if !strings.Contains(got, "key:val") {
		t.Errorf("expected field in output, got: %s", got)
	}
}

func TestWithFields(t *testing.T) {
	l, buf := captureLogger(t, logger.Debug)
	l.WithFields(map[string]any{"a": "1", "b": "2"}).Debug("msg")
	got := buf.String()
	if !strings.Contains(got, "a:1") || !strings.Contains(got, "b:2") {
		t.Errorf("expected both fields in output, got: %s", got)
	}
}

func TestSetLevel(t *testing.T) {
	l, buf := captureLogger(t, logger.Error)

	l.Debug("hidden at error")
	if buf.Len() > 0 {
		t.Fatalf("expected no output at Error level, got: %s", buf.String())
	}
	buf.Reset()

	l.SetLevel(logger.Debug)
	l.Debug("now visible")
	if buf.Len() == 0 {
		t.Fatal("expected Debug output after SetLevel(Debug)")
	}
	if !strings.Contains(buf.String(), "now visible") {
		t.Errorf("expected message in output, got: %s", buf.String())
	}
}

func TestSetOutput(t *testing.T) {
	l, firstBuf := captureLogger(t, logger.Info)
	l.Info("first out")
	if firstBuf.Len() == 0 {
		t.Fatal("expected output in first buffer")
	}

	var secondBuf bytes.Buffer
	l.SetOutput(&secondBuf)
	l.Info("second out")

	if secondBuf.Len() == 0 {
		t.Fatal("expected output in second buffer after SetOutput")
	}
	if !strings.Contains(secondBuf.String(), "second out") {
		t.Errorf("expected redirected message, got: %s", secondBuf.String())
	}
}

func TestDefaultLogger(t *testing.T) {
	// Should not panic
	l := logger.DefaultLogger
	l.Info("default ok")
}

func TestExitFunc(t *testing.T) {
	var exitCode int
	orig := logger.ExitFunc
	logger.ExitFunc = func(code int) { exitCode = code }
	defer func() { logger.ExitFunc = orig }()

	var buf bytes.Buffer
	l := logger.New(logger.WithRecorder(&mockRecorder{}), logger.WithLevel(logger.Fatal), logger.WithOutput(&buf))
	l.Fatal("exit test")

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
	if !strings.Contains(buf.String(), "exit test") {
		t.Errorf("expected fatal message, got: %s", buf.String())
	}
}

func TestContext(t *testing.T) {
	l, buf := captureLogger(t, logger.Info)

	ctx := l.WithContext(context.Background())
	if ctx == nil {
		t.Fatal("expected non-nil context")
	}

	retrieved := logger.FromContext(ctx)
	retrieved.Info("from ctx")
	if buf.Len() == 0 {
		t.Error("expected output from context logger")
	}
}

func TestTraceLevel(t *testing.T) {
	// 走默认 slog 实现验证 [TRAC] 输出
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithLevel(logger.Trace), logger.WithColor(false))
	l.Trace("trace msg")
	if !strings.Contains(buf.String(), "[TRAC] trace msg") {
		t.Errorf("expected trace output, got: %s", buf.String())
	}
}

// --- 新增：默认 slog 实现 ---

func TestNewSlogDefault(t *testing.T) {
	// 无参 New() 不 panic，返回可用 logger
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

func TestSlogWithLog(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false))

	l.With("key", "val").Log(logger.Info, "slog msg")
	got := buf.String()
	if !strings.Contains(got, "slog msg") {
		t.Errorf("expected message in output, got: %s", got)
	}
	if !strings.Contains(got, "key=val") {
		t.Errorf("expected key=val in output, got: %s", got)
	}
}

// --- 新增：slog.Attr 接口能力与 context 注入 ---

func TestWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false))

	l.WithAttrs(slog.String("k", "v")).Info("msg")
	got := buf.String()
	if !strings.Contains(got, "k=v") {
		t.Errorf("expected k=v in output, got: %s", got)
	}
	if !strings.Contains(got, "msg") {
		t.Errorf("expected msg in output, got: %s", got)
	}
}

func TestLogAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false))

	l.LogAttrs(logger.Info, "msg", slog.Int("n", 1))
	got := buf.String()
	if !strings.Contains(got, "msg") {
		t.Errorf("expected msg in output, got: %s", got)
	}
	if !strings.Contains(got, "n=1") {
		t.Errorf("expected n=1 in output, got: %s", got)
	}
}

func TestWithGroup(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false))

	l.WithGroup("g").WithAttrs(slog.String("k", "v")).Info("msg")
	if !strings.Contains(buf.String(), "g.k=v") {
		t.Errorf("expected g.k=v in output, got: %s", buf.String())
	}
}

func TestContextInject(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false))

	ctx := logger.WithContext(context.Background(), l)
	if ctx == nil {
		t.Fatal("expected non-nil context")
	}

	retrieved := logger.FromContext(ctx)
	retrieved.Info("from inject ctx")
	if !strings.Contains(buf.String(), "from inject ctx") {
		t.Errorf("expected output from context logger, got: %s", buf.String())
	}

	// 空 ctx / 无值 ctx 回退 DefaultLogger 不 panic
	if got := logger.FromContext(nil); got == nil {
		t.Fatal("expected fallback logger for nil ctx")
	}
	if got := logger.FromContext(context.Background()); got == nil {
		t.Fatal("expected fallback logger for empty ctx")
	}
}

func TestWithGroupRecorder(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithRecorder(&mockRecorder{}), logger.WithOutput(&buf), logger.WithColor(false), logger.WithLevel(logger.Debug))

	l.WithGroup("g").WithAttrs(slog.String("k", "v")).Debug("msg")
	got := buf.String()
	if !strings.Contains(got, "g.k:v") {
		t.Errorf("expected g.k:v in output, got: %s", got)
	}
	if !strings.Contains(got, "msg") {
		t.Errorf("expected msg in output, got: %s", got)
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
	// 手动创建临时目录：Windows 上 lumberjack 不主动释放文件句柄，
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

func TestFileOutputWithRecorder(t *testing.T) {
	// 手动创建临时目录：Windows 上 lumberjack 不主动释放文件句柄，
	// 若用 t.TempDir 自动清理会因句柄占用而失败（同 TestFileOutput）。
	dir, err := os.MkdirTemp("", "logger-recorder-file-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(dir) // 句柄占用时删除会失败，进程退出后自动释放，忽略错误

	path := filepath.Join(dir, "app.log")

	var buf bytes.Buffer
	l := logger.New(logger.WithRecorder(&mockRecorder{}), logger.WithOutput(&buf), logger.WithFile(path))
	l.Info("hello file")

	// 1. 控制台仍输出
	if buf.Len() == 0 {
		t.Fatal("expected console output with file output enabled")
	}

	// 2. 文件存在且内容包含消息（recorder 输出原样落盘）
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if !strings.Contains(string(data), "hello file") {
		t.Errorf("expected message in file, got: %s", string(data))
	}
}

func TestSetOutputKeepsFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "logger-setoutput-file-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(dir) // 同上，忽略清理错误

	path := filepath.Join(dir, "app.log")

	var buf bytes.Buffer
	l := logger.New(logger.WithRecorder(&mockRecorder{}), logger.WithOutput(&buf), logger.WithFile(path))
	l.Info("before redirect")

	var secondBuf bytes.Buffer
	l.SetOutput(&secondBuf)
	l.Info("after redirect")

	// 文件继续增长：两条消息都在文件中
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if !strings.Contains(string(data), "before redirect") {
		t.Errorf("expected first message in file, got: %s", string(data))
	}
	if !strings.Contains(string(data), "after redirect") {
		t.Errorf("expected second message in file after SetOutput, got: %s", string(data))
	}

	// 新输出目标非空
	if secondBuf.Len() == 0 {
		t.Fatal("expected output in second buffer after SetOutput")
	}
	if !strings.Contains(secondBuf.String(), "after redirect") {
		t.Errorf("expected redirected message, got: %s", secondBuf.String())
	}
}

func TestSetLevelDynamic(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithLevel(logger.Error), logger.WithColor(false))

	l.Debug("hidden")
	if buf.Len() > 0 {
		t.Fatalf("expected no output at Error level, got: %s", buf.String())
	}

	l.SetLevel(logger.Debug)
	l.Debug("now visible")
	if buf.Len() == 0 {
		t.Fatal("expected Debug output after SetLevel(Debug)")
	}
	if !strings.Contains(buf.String(), "now visible") {
		t.Errorf("expected message in output, got: %s", buf.String())
	}
}

// A8：WithFields 输出顺序确定（map key 排序后按序输出，不再随机）
func TestWithFieldsOrder(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false))
	l.WithFields(map[string]any{"z": 1, "a": 2, "m": 3}).Info("msg")

	got := buf.String()
	ai := strings.Index(got, "a=2")
	mi := strings.Index(got, "m=3")
	zi := strings.Index(got, "z=1")
	if ai < 0 || mi < 0 || zi < 0 {
		t.Fatalf("expected all fields in output, got: %s", got)
	}
	if !(ai < mi && mi < zi) {
		t.Errorf("expected sorted order a<m<z, got: %s", got)
	}
}

// A9：helper.Log 的 args 不再静默丢弃——转为 recorder 字段输出
func TestRecorderLogArgs(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithRecorder(&mockRecorder{}), logger.WithOutput(&buf), logger.WithLevel(logger.Debug))
	l.Log(logger.Info, "msg", "k", "v")

	got := buf.String()
	if !strings.Contains(got, "k:v") {
		t.Errorf("expected args as fields in output, got: %s", got)
	}
	if !strings.Contains(got, "msg") {
		t.Errorf("expected msg in output, got: %s", got)
	}
}

// A9：attrsToMap 递归展开 Group——组内 key 用 "group.key" 连接
func TestRecorderLogAttrsGroup(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithRecorder(&mockRecorder{}), logger.WithOutput(&buf), logger.WithLevel(logger.Debug))
	l.LogAttrs(logger.Info, "msg", slog.Group("db", slog.String("user", "u")))

	got := buf.String()
	if !strings.Contains(got, "db.user:u") {
		t.Errorf("expected group key expanded, got: %s", got)
	}
}

// --- mock ---

type mockRecorder struct {
	mu     sync.Mutex
	opt    logger.Options
	fields map[string]any
}

func (m *mockRecorder) Init(opt logger.Options) {
	m.mu.Lock()
	m.opt = opt
	m.mu.Unlock()
}
func (m *mockRecorder) Fields(fields map[string]any) logger.LogRecorder {
	m.mu.Lock()
	cp := make(map[string]any, len(m.fields))
	for k, v := range m.fields {
		cp[k] = v
	}
	opt := m.opt
	m.mu.Unlock()

	for k, v := range fields {
		cp[k] = v
	}
	return &mockRecorder{opt: opt, fields: cp}
}
func (m *mockRecorder) Log(level logger.Level, v ...any) {
	m.mu.Lock()
	out := m.opt.Out
	fields := ""
	if len(m.fields) > 0 {
		parts := make([]string, 0, len(m.fields))
		for k, v := range m.fields {
			parts = append(parts, k+":"+fmt.Sprint(v))
		}
		fields = "[" + strings.Join(parts, " ") + "] "
	}
	s := level.String() + " " + fields + joinStrings(v...) + "\n"
	_, _ = out.Write([]byte(s))
	m.mu.Unlock()
}
func (m *mockRecorder) Logf(level logger.Level, format string, v ...any) {
	m.mu.Lock()
	out := m.opt.Out
	fields := ""
	if len(m.fields) > 0 {
		parts := make([]string, 0, len(m.fields))
		for k, v := range m.fields {
			parts = append(parts, k+":"+fmt.Sprint(v))
		}
		fields = "[" + strings.Join(parts, " ") + "] "
	}
	s := level.String() + " " + fields + fmt.Sprintf(format, v...) + "\n"
	_, _ = out.Write([]byte(s))
	m.mu.Unlock()
}
func (m *mockRecorder) String() string { return "mock" }

func joinStrings(v ...any) string {
	var s string
	for i, a := range v {
		if i > 0 {
			s += " "
		}
		str, ok := a.(string)
		if !ok {
			continue
		}
		s += str
	}
	return s
}

func TestConcurrentSafety(t *testing.T) {
	l := logger.New(logger.WithRecorder(&mockRecorder{}), logger.WithLevel(logger.Debug), logger.WithOutput(blackHole{}))

	var wg sync.WaitGroup

	// Multiple goroutines logging concurrently
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				l.Info("concurrent info")
				l.Debug("concurrent debug")
				l.WithField("key", "val").Warn("with field")
			}
		}()
	}

	// SetLevel and SetOutput concurrently with logging
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			l.SetLevel(logger.Warn)
			l.SetLevel(logger.Debug)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			l.SetOutput(blackHole{})
			l.SetOutput(blackHole{})
		}
	}()

	// WithFields while logging
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				l.WithFields(map[string]any{"k": "v"}).Info("derived")
			}
		}()
	}

	wg.Wait()
}

// blackHole implements io.Writer discarding all data (thread-safe).
type blackHole struct{}

func (blackHole) Write(p []byte) (int, error) { return len(p), nil }
