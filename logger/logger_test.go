package logger_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"git.charlienet.top/go/gadget/logger"
)

func captureLogger(t *testing.T, level logger.Level) (logger.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l := logger.New(&mockRecorder{}, logger.WithLevel(level), logger.WithOutput(&buf))
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
	l := logger.New(&mockRecorder{}, logger.WithLevel(logger.Fatal), logger.WithOutput(&buf))
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
	l, buf := captureLogger(t, logger.Trace)
	l.Trace("trace msg")
	if !strings.Contains(buf.String(), "TRACE trace msg") {
		t.Errorf("expected trace output, got: %s", buf.String())
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
	out.Write([]byte(s))
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
	out.Write([]byte(s))
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
	l := logger.New(&mockRecorder{}, logger.WithLevel(logger.Debug), logger.WithOutput(blackHole{}))

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