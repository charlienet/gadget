package logger_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlienet/gadget/logger"
)

// --- ① Init 端到端 ---
//
// Init 会替换包级 DefaultLogger 并 slog.SetDefault：
// 每个用例用 withRestoreDefault 登记恢复，避免污染其他测试。
// console 输出走 New 默认 Out=os.Stdout（调用时读取），测试期临时重定向 os.Stdout 捕获。

// withRestoreDefault 保存并登记恢复 DefaultLogger / slog 默认实例。
func withRestoreDefault(t *testing.T) {
	t.Helper()
	orig := logger.DefaultLogger
	t.Cleanup(func() {
		logger.DefaultLogger = orig
		slog.SetDefault(orig)
	})
}

// captureStdout 在 fn 执行期间把 os.Stdout 重定向到管道，返回其捕获内容。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read capture pipe: %v", err)
	}
	return string(data)
}

func TestInitConsole(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	got := captureStdout(t, func() {
		if err := logger.Init(logger.Config{
			Level:   "warn",
			Output:  "console",
			Service: "pay-svc",
			Env:     "test",
			Source:  true,
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}

		// 低于 Warn 的级别被过滤
		slog.Info("info should be filtered")
		slog.Warn("console warn msg", "k", "v")
	})

	if strings.Contains(got, "info should be filtered") {
		t.Errorf("expected Info filtered at warn level, got: %s", got)
	}
	if !strings.Contains(got, "console warn msg") {
		t.Errorf("expected Warn output, got: %s", got)
	}
	if !strings.Contains(got, "k=v") {
		t.Errorf("expected attrs in output, got: %s", got)
	}
	if !strings.Contains(got, "service=pay-svc") || !strings.Contains(got, "env=test") {
		t.Errorf("expected service/env attrs, got: %s", got)
	}
	if !strings.Contains(got, "source=") {
		t.Errorf("expected source= with Source:true, got: %s", got)
	}
	// Init 替换 DefaultLogger 且 slog.SetDefault 生效
	if logger.DefaultLogger != slog.Default() {
		t.Error("expected Init to route slog default to the new DefaultLogger")
	}
}

func TestInitFileOnly(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	dir, err := os.MkdirTemp("", "logger-init-file-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(dir) // 同 TestFileOutput：忽略句柄占用错误

	path := filepath.Join(dir, "app.log")

	if err := logger.Init(logger.Config{
		Level:  "info",
		Output: "file", // 纯文件：控制台丢弃
		File:   path,
		Source: false,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// trace 经 DefaultLogger 的 InfoContext 走 handler 链（含 TraceHandler）
	ctx := logger.WithTraceID(context.Background(), "init-file-trace")
	logger.DefaultLogger.InfoContext(ctx, "file only msg")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "file only msg") {
		t.Errorf("expected message in JSON file, got: %s", got)
	}
	if !strings.Contains(got, `"trace_id":"init-file-trace"`) {
		t.Errorf("expected trace_id in JSON file, got: %s", got)
	}
}

func TestInitBoth(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	dir, err := os.MkdirTemp("", "logger-init-both-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "both.log")

	gotConsole := captureStdout(t, func() {
		if err := logger.Init(logger.Config{
			Level:      "trace",
			Output:     "both",
			File:       path,
			MaxSize:    5,
			MaxAge:     1,
			MaxBackups: 2,
			Compress:   false,
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		slog.Debug("both debug msg") // Level=trace 时 Debug 应输出
	})

	if !strings.Contains(gotConsole, "both debug msg") {
		t.Errorf("expected console output at trace level, got: %s", gotConsole)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "both debug msg") {
		t.Errorf("expected file output in both mode, got: %s", string(data))
	}
}

func TestInitAsync(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	var buf bytes.Buffer
	// 直接验证异步 Config 路径：Init 不支持自定义输出，这里用同参数的 New 验证
	// 异步装配来自 Init 语义；再走一遍 Init 确认不报错且 DefaultLogger 可用。
	cfg := logger.Config{Level: "info", Output: "console", Async: true, QueueSize: 64}

	l := logger.New(logger.WithLevel(logger.ParseLevel(cfg.Level)),
		logger.WithOutput(&buf), logger.WithAsync(cfg.QueueSize), logger.WithColor(false))
	l.Info("async via config")
	logger.Close(2 * time.Second) // flush
	if !strings.Contains(buf.String(), "async via config") {
		t.Errorf("expected async flushed output, got: %q", buf.String())
	}

	got := captureStdout(t, func() {
		if err := logger.Init(cfg); err != nil {
			t.Fatalf("Init async: %v", err)
		}
		slog.Info("init async msg")
		logger.Close(2 * time.Second) // 异步：捕获窗口内 flush，确保写入管道后再恢复 stdout
	})
	if !strings.Contains(got, "init async msg") {
		t.Errorf("expected async msg via stdout after Close, got: %q", got)
	}
}

// Init 级别为空时走 LOG_LEVEL 环境变量兜底
func TestInitLevelFromEnvFallback(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	t.Setenv("LOG_LEVEL", "debug")

	got := captureStdout(t, func() {
		if err := logger.Init(logger.Config{Level: "", Output: "console"}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		slog.Debug("env debug visible") // LOG_LEVEL=debug → Debug 不被过滤
		slog.Info("env info visible")
	})
	if !strings.Contains(got, "env debug visible") {
		t.Errorf("expected Debug output via LOG_LEVEL fallback, got: %s", got)
	}

	// 环境变量非法 → 回退 Info，Debug 被过滤
	t.Setenv("LOG_LEVEL", "garbage")
	got = captureStdout(t, func() {
		if err := logger.Init(logger.Config{Output: "console"}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		slog.Debug("hidden debug")
		slog.Info("shown info")
	})
	if strings.Contains(got, "hidden debug") {
		t.Errorf("expected Debug filtered with bogus LOG_LEVEL fallback Info, got: %s", got)
	}
	if !strings.Contains(got, "shown info") {
		t.Errorf("expected Info output, got: %s", got)
	}
}
