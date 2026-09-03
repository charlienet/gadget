package lifecycle

import (
	"log/slog"
	"os"
	"syscall"
	"testing"
	"time"
)

// assertOptPanic 在隔离的 options 上应用一个选项，断言其 panic。
func assertOptPanic(t *testing.T, name string, fn Option) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("%s 期望 panic，实际无", name)
		}
	}()
	o := &options{}
	fn(o)
}

// assertNoPanic 断言 fn 不触发 panic。
func assertNoPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s 期望不 panic，实得 %v", name, r)
		}
	}()
	fn()
}

func TestOptionValidation(t *testing.T) {
	assertOptPanic(t, "WithStepTimeout(0)", WithStepTimeout(0))
	assertOptPanic(t, "WithStepTimeout(-1)", WithStepTimeout(-1))
	assertOptPanic(t, "WithTotalTimeout(0)", WithTotalTimeout(0))
	assertOptPanic(t, "WithTotalTimeout(-1)", WithTotalTimeout(-1))
	assertOptPanic(t, "WithSignals()", WithSignals())
	assertOptPanic(t, "WithSignals(nil)", WithSignals([]os.Signal{nil}...))
	assertOptPanic(t, "WithLogger(nil)", WithLogger(nil))

	// 合法配置不应 panic（反向断言）。
	assertNoPanic(t, "valid options", func() {
		o := &options{stepTimeout: defaultStepTimeout}
		WithStepTimeout(time.Second)(o)
		WithTotalTimeout(2 * time.Second)(o)
		WithSignals(syscall.SIGTERM)(o)
		WithLogger(slog.New(slog.NewTextHandler(nopWriter{}, nil)))(o)
	})
}

// New 时统一校验：非法选项应在 New 处 panic。
func TestNewAppliesValidation(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
	}{
		{"step", []Option{WithStepTimeout(0)}},
		{"total", []Option{WithTotalTimeout(-1)}},
		{"signals", []Option{WithSignals()}},
		{"logger", []Option{WithLogger(nil)}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("New(%s) 期望 panic", tc.name)
				}
			}()
			_ = New(tc.opts...)
		})
	}
}

func TestOptionsDefaultsAndApply(t *testing.T) {
	// 默认值：stepTimeout=5s、totalTimeout=0、signals 含 SIGTERM/SIGINT、logger=nil。
	m := New()
	if m.opts.stepTimeout != 5*time.Second {
		t.Fatalf("默认 stepTimeout = %v, want 5s", m.opts.stepTimeout)
	}
	if m.opts.totalTimeout != 0 {
		t.Fatalf("默认 totalTimeout = %v, want 0", m.opts.totalTimeout)
	}
	if m.opts.logger != nil {
		t.Fatal("默认 logger 应为 nil")
	}
	if len(m.opts.signals) != 2 {
		t.Fatalf("默认 signals 数量 = %d, want 2", len(m.opts.signals))
	}

	// 显式设置生效。
	lg := slog.New(slog.NewTextHandler(nopWriter{}, nil))
	m2 := New(
		WithStepTimeout(time.Second),
		WithTotalTimeout(3*time.Second),
		WithSignals(syscall.SIGQUIT),
		WithLogger(lg),
	)
	if m2.opts.stepTimeout != time.Second {
		t.Fatalf("stepTimeout = %v, want 1s", m2.opts.stepTimeout)
	}
	if m2.opts.totalTimeout != 3*time.Second {
		t.Fatalf("totalTimeout = %v, want 3s", m2.opts.totalTimeout)
	}
	if len(m2.opts.signals) != 1 || m2.opts.signals[0] != syscall.SIGQUIT {
		t.Fatalf("signals = %v, want [SIGQUIT]", m2.opts.signals)
	}
	if m2.opts.logger != lg {
		t.Fatal("logger 未被设置")
	}
}

// nopWriter 丢弃所有写入，供构造 logger 用。
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
