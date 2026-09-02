package logger_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/charlienet/gadget/logger"
)

// --- ④ AsyncHandler：WithAttrs/WithGroup 派生、共享队列、Close 边界 ---

func TestAsyncHandlerDerivedHandlers(t *testing.T) {
	var buf bytes.Buffer
	h := logger.NewAsyncHandler(slog.NewTextHandler(&buf, nil), 8, false)

	// WithGroup 派生：分组在消费时由底层 handler 应用
	dg := h.WithGroup("g")
	r1 := slog.NewRecord(time.Now(), slog.LevelInfo, "grouped", 0)
	r1.AddAttrs(slog.String("k", "v"))
	if err := dg.Handle(context.Background(), r1); err != nil {
		t.Fatalf("grouped Handle: %v", err)
	}

	// WithAttrs 派生：预设属性在消费时合并
	da := h.WithAttrs([]slog.Attr{slog.String("base", "1")})
	r2 := slog.NewRecord(time.Now(), slog.LevelInfo, "preset", 0)
	if err := da.Handle(context.Background(), r2); err != nil {
		t.Fatalf("attrs Handle: %v", err)
	}

	// 派生实例共享同一队列（Stats 计入派生写入）
	if total, _ := h.Stats(); total != 2 {
		t.Errorf("expected derived handlers to share queue, total=%d", total)
	}

	if err := h.Close(2 * time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "g.k=v") {
		t.Errorf("expected group applied via async, got: %q", got)
	}
	if !strings.Contains(got, "base=1") {
		t.Errorf("expected preset attrs via async, got: %q", got)
	}
}

func TestAsyncHandlerCloseEdges(t *testing.T) {
	// queueSize<=0 默认容量分支 + timeout<=0 默认 2s 分支
	h := logger.NewAsyncHandler(slog.NewTextHandler(io.Discard, nil), 0, false)
	h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "q", 0))
	if err := h.Close(0); err != nil {
		t.Fatalf("Close(0): %v", err)
	}

	// 重复 Close：已关闭直接返回 nil（幂等）
	if err := h.Close(0); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// Close 后 Handle 静默丢弃：返回 nil 且 total 不再增长
	totalBefore, _ := h.Stats()
	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "after", 0)); err != nil {
		t.Errorf("Handle after Close must return nil, got %v", err)
	}
	totalAfter, _ := h.Stats()
	if totalBefore != 1 || totalAfter != 1 {
		t.Errorf("expected total stays 1 after close, before=%d after=%d", totalBefore, totalAfter)
	}
}

// --- ④ 采样装饰器：WithAttrs/WithGroup 派生后属性仍生效 ---

func TestSamplingDerivedHandlers(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false),
		logger.WithSampling(100, 100)) // 阈值放宽：只验证派生透传，不验证丢弃

	l.With("k", "v").Info("sampled derived")
	if !strings.Contains(buf.String(), "k=v") {
		t.Errorf("expected preset attr through sampling, got: %s", buf.String())
	}

	buf.Reset()
	l.WithGroup("g").Info("sampled group", "x", 1)
	got := buf.String()
	if !strings.Contains(got, "g.x=1") {
		t.Errorf("expected group prefix through sampling chain, got: %s", got)
	}
}

// --- ④ 敏感装饰器：WithAttrs 预设打码 / WithGroup 组内递归打码 ---

func TestSensitiveDerivedHandlers(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false),
		logger.WithSensitiveKeys("password"))

	// With 预设的敏感属性同样打码（sensitiveHandler.WithAttrs）
	l.With("password", "raw-pw").Info("preset")
	got := buf.String()
	if strings.Contains(got, "raw-pw") || !strings.Contains(got, "******") {
		t.Errorf("expected preset sensitive masked, got: %s", got)
	}

	buf.Reset()
	// WithGroup 后组内敏感 key 打码（WithGroup 派生 + Group 递归匹配）
	l.WithGroup("db").Info("grouped", "password", "inner-pw")
	got = buf.String()
	if strings.Contains(got, "inner-pw") || !strings.Contains(got, "db.password=******") {
		t.Errorf("expected grouped sensitive masked, got: %s", got)
	}

	// Group 属性值含多个敏感字段：递归全打码（attrHasSensitive 提前终止分支）
	buf.Reset()
	l.With("unused", "x") // 触发一次带预设的派生，确保后续 Handle 走已装饰链
	l.Info("multi", slog.Group("auth",
		slog.String("token", "tk_val"),
		slog.String("password", "pd_val"),
		slog.String("user", "keep_me"),
	))
	got = buf.String()
	if strings.Contains(got, "tk_val") || strings.Contains(got, "pd_val") {
		t.Errorf("expected all nested sensitive masked, got: %s", got)
	}
	if !strings.Contains(got, "keep_me") {
		t.Errorf("expected non-sensitive kept, got: %s", got)
	}
}

// --- ④ 错误堆栈装饰器：Unwrap / Wrap 边界 / WithGroup 透传 ---

func TestWrapUnwrapChain(t *testing.T) {
	sentinel := errors.New("sentinel")
	wrapped := logger.Wrap(sentinel)

	if !errors.Is(wrapped, sentinel) {
		t.Error("errors.Is must find sentinel through Wrap")
	}
	if errors.Unwrap(wrapped) != sentinel {
		t.Error("Unwrap must return original error")
	}
	st, ok := wrapped.(logger.StackTracer)
	if !ok || len(st.StackTrace()) == 0 {
		t.Error("wrapped error must implement StackTracer with non-empty stack")
	}
	if logger.Wrap(nil) != nil {
		t.Error("Wrap(nil) must return nil")
	}
	// 双层 Wrap：Unwrap 链完整
	twice := logger.Wrap(wrapped)
	if !errors.Is(twice, sentinel) {
		t.Error("double Wrap must keep errors.Is chain")
	}
}

func TestStackHandlerWithGroup(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithStackTrace(true))

	err := logger.Wrap(errors.New("boom"))
	l.WithGroup("e").Error("failed", logger.Err(err))

	got := buf.String()
	if !strings.Contains(got, "e.error=boom") {
		t.Errorf("expected grouped error attr, got: %s", got)
	}
	// 组内 stack 属性 key 同样带分组前缀；栈帧应指向本文件（Wrap 调用处）
	if !strings.Contains(got, "e.error_stack=") {
		t.Errorf("expected grouped error_stack attr, got: %s", got)
	}
	if !strings.Contains(got, "decorators_test.go") {
		t.Errorf("expected stack frame pointing to this test file, got: %s", got)
	}
}

// 普通 error（未 Wrap）与非 KindAny 属性：不追加 stack（stackTracerOf 双 false 分支）
func TestStackSkipsPlainValues(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithStackTrace(true))

	l.Error("plain", "err", errors.New("not wrapped"), "count", 5, "name", "n")
	got := buf.String()
	if strings.Contains(got, "_stack") {
		t.Errorf("expected no stack for plain values, got: %s", got)
	}
	if !strings.Contains(got, "err=not wrapped") || !strings.Contains(got, "count=5") {
		t.Errorf("expected plain attrs kept, got: %s", got)
	}
}

// --- Fatal 经异步默认 logger：flushAsync 的 async!=nil 分支 ---

func TestFatalFlushesAsyncDefault(t *testing.T) {
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	var exitCode int
	origExit := logger.ExitFunc
	logger.ExitFunc = func(code int) { exitCode = code }
	defer func() { logger.ExitFunc = origExit }()

	origDefault := logger.DefaultLogger
	defer func() { logger.DefaultLogger = origDefault }()

	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithAsync(64), logger.WithColor(false))
	logger.DefaultLogger = l

	logger.Fatal("fatal async msg")

	if exitCode != 1 {
		t.Errorf("expected exit 1, got %d", exitCode)
	}
	// 未经包级 Close：Fatal 内部已 flush 异步队列
	if !strings.Contains(buf.String(), "[FATA] fatal async msg") {
		t.Errorf("expected fatal msg flushed synchronously by Fatal, got: %q", buf.String())
	}
}
