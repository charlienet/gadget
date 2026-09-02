package logger_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/charlienet/gadget/logger"
)

// Err(nil) 返回空 Attr（Key 为空），Err(err) 返回 key="error"
func TestErrAttr(t *testing.T) {
	if a := logger.Err(nil); a.Key != "" {
		t.Errorf("expected empty attr for nil error, got key=%q", a.Key)
	}

	err := errors.New("boom")
	if a := logger.Err(err); a.Key != "error" {
		t.Errorf("expected key=error, got key=%q", a.Key)
	}
}

// WithStackTrace 启用时，Wrap 过的错误自动附加 stack 属性
func TestWrapStackTrace(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithStackTrace(true))
	l.Error("failed", logger.Err(logger.Wrap(errors.New("boom"))))

	got := buf.String()
	if !strings.Contains(got, "error") {
		t.Errorf("expected error attr in output, got: %s", got)
	}
	if !strings.Contains(got, "stack") {
		t.Errorf("expected stack attr in output, got: %s", got)
	}
	// 栈非空且包含创建位置（本测试文件）
	if !strings.Contains(got, "err_test.go") {
		t.Errorf("expected stack frames in output, got: %s", got)
	}
}

// 默认不启用时，不附加 stack 属性
func TestNoStackWhenDisabled(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false))
	l.Error("failed", logger.Err(logger.Wrap(errors.New("boom"))))

	if got := buf.String(); strings.Contains(got, "stack") {
		t.Errorf("expected no stack attr by default, got: %s", got)
	}
}

// WithAttrs 预设的 Err 属性同样附加堆栈（A1：stackHandler.WithAttrs 与 Handle 逻辑对称）
func TestStackViaWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithStackTrace(true))
	l.With(logger.Err(logger.Wrap(errors.New("boom")))).Error("msg")

	got := buf.String()
	if !strings.Contains(got, "stack") {
		t.Errorf("expected stack attr in output for WithAttrs preset, got: %s", got)
	}
	if !strings.Contains(got, "error") {
		t.Errorf("expected error attr in output, got: %s", got)
	}
}
