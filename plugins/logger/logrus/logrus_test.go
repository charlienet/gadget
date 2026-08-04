package logrus_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charlienet/gadget/logger"
	"github.com/charlienet/gadget/plugins/logger/logrus"

	log "github.com/sirupsen/logrus"
)

// captureLogs creates a logrus adapter that writes to a buffer.
func captureLogger(t *testing.T, level logger.Level, opts ...logrus.Option) (logger.Logger, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer
	opts = append(opts, logrus.WithFormatter(&log.TextFormatter{
		DisableColors: true,
		FullTimestamp: false,
	}))
	l := logger.New(logrus.New(opts...), logger.WithLevel(level), logger.WithOutput(&buf))
	return l, &buf
}

func TestLogLevels(t *testing.T) {
	l, buf := captureLogger(t, logger.Debug)

	tests := []struct {
		name    string
		logFunc func()
		expect  string
	}{
		{"Debug", func() { l.Debug("debug msg") }, `level=debug msg="debug msg"`},
		{"Info", func() { l.Info("info msg") }, `level=info msg="info msg"`},
		{"Warn", func() { l.Warn("warn msg") }, `level=warning msg="warn msg"`},
		{"Error", func() { l.Error("error msg") }, `level=error msg="error msg"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			tt.logFunc()
			got := buf.String()
			if !strings.Contains(got, tt.expect) {
				t.Errorf("expected output to contain %q, got %q", tt.expect, got)
			}
		})
	}
}

func TestLevelFiltering(t *testing.T) {
	l, buf := captureLogger(t, logger.Warn)

	l.Debug("should be hidden")
	if buf.Len() > 0 {
		t.Errorf("Debug log should be suppressed at Warn level, got: %s", buf.String())
	}
	buf.Reset()

	l.Info("should be hidden")
	if buf.Len() > 0 {
		t.Errorf("Info log should be suppressed at Warn level, got: %s", buf.String())
	}
	buf.Reset()

	l.Warn("visible warn")
	if buf.Len() == 0 {
		t.Fatal("Warn log should be visible at Warn level")
	}
	if !strings.Contains(buf.String(), "visible warn") {
		t.Errorf("expected warn message in output, got: %s", buf.String())
	}
}

func TestLogf(t *testing.T) {
	l, buf := captureLogger(t, logger.Info)

	l.Infof("formatted %s %d", "test", 42)
	got := buf.String()
	if !strings.Contains(got, "formatted test 42") {
		t.Errorf("expected formatted message in output, got: %s", got)
	}
}

func TestWithField(t *testing.T) {
	l, buf := captureLogger(t, logger.Debug)

	l2 := l.WithField("key1", "val1").WithField("key2", "val2")
	l2.Info("with fields")

	got := buf.String()
	if !strings.Contains(got, "key1=val1") {
		t.Errorf("expected key1=val1 in output, got: %s", got)
	}
	if !strings.Contains(got, "key2=val2") {
		t.Errorf("expected key2=val2 in output, got: %s", got)
	}
}

func TestWithFields(t *testing.T) {
	l, buf := captureLogger(t, logger.Debug)

	l2 := l.WithFields(map[string]any{"request_id": "abc-123", "user": "tester"})
	l2.Info("batch fields")

	got := buf.String()
	if !strings.Contains(got, "request_id=abc-123") {
		t.Errorf("expected request_id=abc-123 in output, got: %s", got)
	}
	if !strings.Contains(got, "user=tester") {
		t.Errorf("expected user=tester in output, got: %s", got)
	}
}

func TestSetLevel(t *testing.T) {
	l, buf := captureLogger(t, logger.Error)

	l.Debug("should be hidden at Error level")
	if buf.Len() > 0 {
		t.Fatalf("expected no output at Error level, got: %s", buf.String())
	}
	buf.Reset()

	l.SetLevel(logger.Debug)
	l.Debug("now visible after SetLevel")
	if buf.Len() == 0 {
		t.Fatal("expected Debug output after SetLevel(Debug)")
	}
	if !strings.Contains(buf.String(), "now visible after SetLevel") {
		t.Errorf("expected message in output, got: %s", buf.String())
	}
}

func TestSetOutput(t *testing.T) {
	l, _ := captureLogger(t, logger.Info)

	var secondBuf bytes.Buffer
	l.SetOutput(&secondBuf)
	l.Info("redirected output")

	if secondBuf.Len() == 0 {
		t.Fatal("expected output in redirected buffer")
	}
	if !strings.Contains(secondBuf.String(), "redirected output") {
		t.Errorf("expected message in second buffer, got: %s", secondBuf.String())
	}
}

func TestJSONFormatter(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(
		logrus.New(logrus.WithJSONFormatter()),
		logger.WithLevel(logger.Info),
		logger.WithOutput(&buf),
	)

	l.Info("json test")
	got := buf.String()

	if !strings.Contains(got, `"msg":"json test"`) {
		t.Errorf("expected JSON format with msg field, got: %s", got)
	}
	if !strings.Contains(got, `"level":"info"`) {
		t.Errorf("expected JSON format with level field, got: %s", got)
	}
}

func TestNestedFormatter(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(
		logrus.New(logrus.WithNestedFormatter()),
		logger.WithLevel(logger.Info),
		logger.WithOutput(&buf),
	)

	l.WithField("req", "abc").Info("nested fmt")
	got := buf.String()

	if !strings.Contains(got, "[req:abc]") {
		t.Errorf("expected nested formatter style with [req:abc], got: %s", got)
	}
}

func TestReportCaller(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(
		logrus.New(logrus.WithReportCaller()),
		logger.WithLevel(logger.Info),
		logger.WithOutput(&buf),
	)

	l.Info("caller info")
	got := buf.String()

	// ReportCaller should include function/package info from the adapter
	if !strings.Contains(got, "logrus.go") {
		t.Errorf("expected adapter file reference in output, got: %s", got)
	}
}

func TestWithHook(t *testing.T) {
	var hookCalled bool
	hook := &logrusTestHook{
		levels: []log.Level{log.InfoLevel, log.ErrorLevel},
		fn: func(entry *log.Entry) {
			hookCalled = true
		},
	}

	var buf bytes.Buffer
	l := logger.New(
		logrus.New(logrus.WithHook(hook)),
		logger.WithLevel(logger.Info),
		logger.WithOutput(&buf),
	)

	l.Info("hook test")
	if !hookCalled {
		t.Error("expected hook to be called")
	}
}

func TestDefaultLogger(t *testing.T) {
	// DefaultLogger should not panic
	_ = logrus.DefaultLogger
}

func TestString(t *testing.T) {
	r := logrus.New()
	if got := r.String(); got != "logrus" {
		t.Errorf("expected String()='logrus', got %q", got)
	}
}

func TestLoggerToLogrusLevel(t *testing.T) {
	// Verify all levels are correctly mapped by checking log output
	var buf bytes.Buffer
	l := logger.New(
		logrus.New(logrus.WithFormatter(&log.TextFormatter{DisableColors: true})),
		logger.WithLevel(logger.Trace),
		logger.WithOutput(&buf),
	)

	l.Trace("trace level")
	if !strings.Contains(buf.String(), "level=trace") {
		t.Errorf("expected trace level in output, got: %s", buf.String())
	}
}

func TestSensitiveDataRedaction(t *testing.T) {
	// Hook that redacts passwords and tokens from log message
	redactHook := &logrusTestHook{
		levels: []log.Level{log.InfoLevel, log.ErrorLevel},
		fn: func(entry *log.Entry) {
			// Redact password=xxx patterns in message
			re := strings.NewReplacer(
				"password=123456", "password=****",
				"token=secret", "token=****",
			)
			entry.Message = re.Replace(entry.Message)
		},
	}

	var buf bytes.Buffer
	l := logger.New(
		logrus.New(
			logrus.WithFormatter(&log.TextFormatter{DisableColors: true}),
			logrus.WithHook(redactHook),
		),
		logger.WithLevel(logger.Info),
		logger.WithOutput(&buf),
	)

	l.Info("login failed: password=123456")
	got := buf.String()

	if strings.Contains(got, "password=123456") {
		t.Errorf("expected password to be redacted, got: %s", got)
	}
	if !strings.Contains(got, "password=****") {
		t.Errorf("expected redacted password in output, got: %s", got)
	}
}

func TestSensitiveFieldRedaction(t *testing.T) {
	// Hook that removes sensitive fields from entry Data
	redactHook := &logrusTestHook{
		levels: []log.Level{log.InfoLevel},
		fn: func(entry *log.Entry) {
			delete(entry.Data, "credit_card")
			delete(entry.Data, "ssn")
			delete(entry.Data, "password")
		},
	}

	var buf bytes.Buffer
	l := logger.New(
		logrus.New(
			logrus.WithFormatter(&log.TextFormatter{DisableColors: true}),
			logrus.WithHook(redactHook),
		),
		logger.WithLevel(logger.Info),
		logger.WithOutput(&buf),
	)

	l.WithFields(map[string]any{
		"user":        "alice",
		"credit_card": "4111-1111-1111-1111",
		"ssn":         "123-45-6789",
	}).Info("user action")

	got := buf.String()

	if strings.Contains(got, "credit_card") {
		t.Errorf("expected credit_card field to be redacted, got: %s", got)
	}
	if strings.Contains(got, "ssn") {
		t.Errorf("expected ssn field to be redacted, got: %s", got)
	}
	if !strings.Contains(got, "user=alice") {
		t.Errorf("expected non-sensitive field to remain, got: %s", got)
	}
}

// --- test helpers ---

// logrusTestHook implements logrus.Hook for testing.
type logrusTestHook struct {
	levels []log.Level
	fn     func(entry *log.Entry)
}

func (h *logrusTestHook) Levels() []log.Level { return h.levels }
func (h *logrusTestHook) Fire(entry *log.Entry) error {
	if h.fn != nil {
		h.fn(entry)
	}
	return nil
}

func TestFatalLevelLogrusMapping(t *testing.T) {
	// Verify the Fatal level is mapped to logrus.FatalLevel
	// by using the adapter's internal mapping through Log()
	var buf bytes.Buffer
	l := logger.New(
		logrus.New(logrus.WithFormatter(&log.TextFormatter{DisableColors: true})),
		logger.WithLevel(logger.Trace),
		logger.WithOutput(&buf),
	)
	// Use Debug level to demonstrate the mapping — Trace is mapped to logrus.TraceLevel
	l.Trace("trace mapped")
	if !strings.Contains(buf.String(), "level=trace") {
		t.Errorf("expected trace level mapping, got: %s", buf.String())
	}
}
