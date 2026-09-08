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

	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read capture pipe: %v", err)
	}
	return string(data)
}

func TestInitConsole(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

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
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	dir, err := os.MkdirTemp("", "logger-init-file-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(dir) // 同 TestFileOutput：忽略句柄占用错误
	}()

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

// TestInitFileTextFormat：Config.FileFormat="text" 透传至文件 handler，落地为 TextHandler 文本格式。
func TestInitFileTextFormat(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	dir, err := os.MkdirTemp("", "logger-init-file-text-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(dir) // 忽略句柄占用错误
	}()

	path := filepath.Join(dir, "app.log")

	if err := logger.Init(logger.Config{
		Level:      "info",
		Output:     "file", // 纯文件：控制台丢弃
		File:       path,
		FileFormat: "text", // 关键：文件用 TextHandler
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	ctx := logger.WithTraceID(context.Background(), "init-file-text")
	logger.DefaultLogger.InfoContext(ctx, "file text msg")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	got := string(data)
	if strings.HasPrefix(strings.TrimSpace(got), "{") {
		t.Errorf("expected text format file, got JSON-looking: %s", got)
	}
	if !strings.Contains(got, "level=INFO") || !strings.Contains(got, "file text msg") {
		t.Errorf("expected TextHandler key=value output, got: %s", got)
	}
	// 自研 text handler：trace_id 以 key=value 形态出现，且前置于 msg 之前
	// （区别于标准 slog TextHandler 把 trace_id 落在 attrs 末尾的固定顺序）
	traceIdx := strings.Index(got, "trace_id=init-file-text")
	msgIdx := strings.Index(got, "msg=")
	if traceIdx < 0 {
		t.Errorf("expected trace_id in text file, got: %s", got)
	}
	if traceIdx >= 0 && msgIdx >= 0 && traceIdx > msgIdx {
		t.Errorf("expected trace_id before msg (custom ordering), got: %s", got)
	}
}

func TestInitBoth(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	dir, err := os.MkdirTemp("", "logger-init-both-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(dir)
	}()

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
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	var buf bytes.Buffer
	// 直接验证异步 Config 路径：Init 不支持自定义输出，这里用同参数的 New 验证
	// 异步装配来自 Init 语义；再走一遍 Init 确认不报错且 DefaultLogger 可用。
	cfg := logger.Config{Level: "info", Output: "console", Async: true, QueueSize: 64}

	l := logger.New(logger.WithLevel(logger.ParseLevel(cfg.Level)),
		logger.WithConsole(logger.WithConsoleWriter(&buf)), logger.WithAsync(cfg.QueueSize), logger.WithConsole(logger.WithConsoleColor(false)))
	l.Info("async via config")
	_ = logger.Close(2 * time.Second) // flush
	if !strings.Contains(buf.String(), "async via config") {
		t.Errorf("expected async flushed output, got: %q", buf.String())
	}

	got := captureStdout(t, func() {
		if err := logger.Init(cfg); err != nil {
			t.Fatalf("Init async: %v", err)
		}
		slog.Info("init async msg")
		_ = logger.Close(2 * time.Second) // 异步：捕获窗口内 flush，确保写入管道后再恢复 stdout
	})
	if !strings.Contains(got, "init async msg") {
		t.Errorf("expected async msg via stdout after Close, got: %q", got)
	}
}

// Init 级别为空时走 LOG_LEVEL 环境变量兜底
func TestInitLevelFromEnvFallback(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

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

// TestInitVariadicOverride：Init(cfg, opts...) 合并语义——
// ① 用户 Option 覆盖 Config 同类项（WithLevel 压过 Config.Level）；
// ② 用户 Option 叠加 Config 未设的能力（WithSensitiveKeys 生效）。
func TestInitVariadicOverride(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	// ① Config level=info 打底，opts WithLevel(warn) 覆盖 → Info 被过滤、Warn 可见
	got := captureStdout(t, func() {
		if err := logger.Init(
			logger.Config{Level: "info", Output: "console"},
			logger.WithLevel(logger.Warn),
		); err != nil {
			t.Fatalf("Init: %v", err)
		}
		slog.Info("info-should-be-filtered")
		slog.Warn("warn-visible")
	})
	if strings.Contains(got, "info-should-be-filtered") {
		t.Errorf("WithLevel(Warn) should override Config level=info, got: %s", got)
	}
	if !strings.Contains(got, "warn-visible") {
		t.Errorf("expected Warn visible after override, got: %s", got)
	}

	// ② Config 无敏感配置，opts WithSensitiveKeys("mytoken") 叠加生效 → 值被打码
	got2 := captureStdout(t, func() {
		if err := logger.Init(
			logger.Config{Level: "info", Output: "console"},
			logger.WithSensitiveKeys("mytoken"),
		); err != nil {
			t.Fatalf("Init: %v", err)
		}
		slog.Info("sens", "mytoken", "raw-secret")
	})
	if strings.Contains(got2, "raw-secret") || !strings.Contains(got2, "******") {
		t.Errorf("expected WithSensitiveKeys via opts to mask value, got: %s", got2)
	}
}

// TestInitSinkValidationAfterMerge：黑洞校验后移到合并之后——
// 反例（file/both 无路径）报错且不改 DefaultLogger；正例（opts WithFile 补路径）通过；
// 非法 FileFormat（非空未知值）报错不静默回退。
func TestInitSinkValidationAfterMerge(t *testing.T) {
	withRestoreDefault(t)
	orig := logger.DefaultLogger

	// 反例1：Output=file 但无路径、无 opts 补 → 报错
	if err := logger.Init(logger.Config{Output: "file", File: ""}); err == nil ||
		!strings.Contains(err.Error(), "requires non-empty file path") {
		t.Errorf(`Init(file, empty) must error, got: %v`, err)
	}
	if logger.DefaultLogger != orig {
		t.Error("failed Init must not touch DefaultLogger")
	}
	// 反例2：both 空路径 → 报错
	if err := logger.Init(logger.Config{Output: "both", File: ""}); err == nil {
		t.Error(`Init(both, empty) must error`)
	}

	// 正例：cfg file 空，但用户 opts WithFile 补路径 → 合并后 sink 有效，通过
	dir := t.TempDir()
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })
	if err := logger.Init(
		logger.Config{Output: "file", File: ""},
		logger.WithFile(filepath.Join(dir, "via-opt.log")),
	); err != nil {
		t.Errorf("WithFile via opts should satisfy file sink, got: %v", err)
	}

	// 反例3：非法 FileFormat（非空未知值）→ 报错，不静默回退 JSON
	if err := logger.Init(logger.Config{
		Output: "file", File: filepath.Join(dir, "x.log"), FileFormat: "yaml",
	}); err == nil || !strings.Contains(err.Error(), "unknown file format") {
		t.Errorf("Init with illegal FileFormat must error, got: %v", err)
	}
}

// --- Config.NoColor 两态端到端（仅控制台 sink 生效）---
//
// captureStdout 把 os.Stdout 重定向到管道（非 TTY）。NoColor=true 强制关色、NoColor=false
// 走自动判定（非 TTY → 关），二者在非 TTY 下输出都无 ANSI——端到端只验证「不涂色」契约；
// 「强制关 vs 自动关」的真实区分在映射层 internal 测试锁死（见 config_internal_test.go）。

func TestInitConfigNoColorForceOff(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	got := captureStdout(t, func() {
		if err := logger.Init(logger.Config{Level: "info", Output: "console", NoColor: true}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		slog.Info("force color off")
	})
	if strings.Contains(got, "\033[") {
		t.Errorf("NoColor=true must suppress ANSI, got: %q", got)
	}
}

func TestInitConfigNoColorAuto(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	got := captureStdout(t, func() {
		// NoColor 省略（false 默认）→ 自动判定：管道非 TTY → 关闭颜色
		if err := logger.Init(logger.Config{Level: "info", Output: "console"}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		slog.Info("auto color")
	})
	if strings.Contains(got, "\033[") {
		t.Errorf("NoColor=false must auto-disable ANSI on non-TTY pipe, got: %q", got)
	}
}

func TestInitConfigNoColorBothFileNoRegression(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	dir, err := os.MkdirTemp("", "logger-nocolor-both-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "app.log")

	got := captureStdout(t, func() {
		if err := logger.Init(logger.Config{Level: "info", Output: "both", File: path, NoColor: true}); err != nil {
			t.Fatalf("Init(both): %v", err)
		}
		slog.Info("both sinks")
	})
	if strings.Contains(got, "\033[") {
		t.Errorf("NoColor=true must suppress console ANSI in both mode, got: %q", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !strings.Contains(string(data), "both sinks") {
		t.Errorf("file sink must still write in both mode, got: %s", data)
	}
}

// --- Config.Layout / Sensitive_Keys / Sensitive_Mask 端到端映射 ---

// TestInitConfigSensitiveKeys：Config.Sensitive_Keys 非空 → 注入 WithSensitiveKeys，
// console 端命中的属性值被打码为默认掩码 ******。
func TestInitConfigSensitiveKeys(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	got := captureStdout(t, func() {
		if err := logger.Init(logger.Config{
			Level:          "info",
			Output:         "console",
			Sensitive_Keys: []string{"mytoken"},
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		slog.Info("sens", "mytoken", "raw-secret", "safe", "keep-me")
	})
	if strings.Contains(got, "raw-secret") || !strings.Contains(got, "******") {
		t.Errorf("expected Config.Sensitive_Keys to mask value, got: %s", got)
	}
	if !strings.Contains(got, "keep-me") {
		t.Errorf("non-sensitive attr must stay intact, got: %s", got)
	}
}

// TestInitConfigSensitiveMask：Config.Sensitive_Mask 非空 → 注入 WithSensitiveMask，
// 自定义掩码替换默认 ******。
func TestInitConfigSensitiveMask(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	got := captureStdout(t, func() {
		if err := logger.Init(logger.Config{
			Level:          "info",
			Output:         "console",
			Sensitive_Keys: []string{"mytoken"},
			Sensitive_Mask: "[X]",
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		slog.Info("sens", "mytoken", "raw-secret")
	})
	if strings.Contains(got, "raw-secret") || !strings.Contains(got, "[X]") {
		t.Errorf("expected custom mask [X] applied, got: %s", got)
	}
	if strings.Contains(got, "******") {
		t.Errorf("default mask must be overridden by Sensitive_Mask, got: %s", got)
	}
}

// TestInitConfigSensitiveMaskOnly：仅设 Sensitive_Mask 不设 keys → WithSensitiveMask
// 初始化 Sensitive options，打码走内置词集（与直接用 Option 行为一致，不发明组合规则）。
func TestInitConfigSensitiveMaskOnly(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	got := captureStdout(t, func() {
		if err := logger.Init(logger.Config{
			Level:          "info",
			Output:         "console",
			Sensitive_Mask: "[M]",
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		slog.Info("sens", "password", "raw-secret") // password 命中内置词集
	})
	if strings.Contains(got, "raw-secret") || !strings.Contains(got, "[M]") {
		t.Errorf("mask-only must still redact builtin keys, got: %s", got)
	}
}

// TestInitConfigSensitiveKeysAppend：Config.Sensitive_Keys 打底 + opts WithSensitiveKeys
// 追加（append 语义）→ 两组词都生效。
func TestInitConfigSensitiveKeysAppend(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	got := captureStdout(t, func() {
		if err := logger.Init(
			logger.Config{
				Level:          "info",
				Output:         "console",
				Sensitive_Keys: []string{"mytoken"},
			},
			logger.WithSensitiveKeys("extra"),
		); err != nil {
			t.Fatalf("Init: %v", err)
		}
		slog.Info("sens", "mytoken", "raw-a", "extra", "raw-b")
	})
	if strings.Contains(got, "raw-a") || strings.Contains(got, "raw-b") {
		t.Errorf("both cfg + opts sensitive keys must apply (append semantics), got: %s", got)
	}
}

// TestInitConfigLayoutDateRotate：Config.Layout 非空 + Output both → 构造成功，
// 落地为按日期命名文件（对齐 rotate 命名规则 name.<layout>.ext）。
func TestInitConfigLayoutDateRotate(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { _ = logger.Close(2 * time.Second) })

	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	got := captureStdout(t, func() {
		if err := logger.Init(logger.Config{
			Level:  "info",
			Output: "both",
			File:   path,
			Layout: "2006-01-02",
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		logger.DefaultLogger.Info("layout rotate msg")
	})
	if !strings.Contains(got, "layout rotate msg") {
		t.Errorf("both mode console output expected, got: %s", got)
	}

	want := filepath.Join(dir, "app."+time.Now().Format("2006-01-02")+".log")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected date-rotated file %s: %v", filepath.Base(want), err)
	}
	// 原始 app.log 不应产生：Layout 非空走日期轮换 writer，文件名已插入日期段
	if _, err := os.Stat(path); err == nil {
		t.Errorf("plain app.log must not be created under date rotate, found at %s", path)
	}
}
