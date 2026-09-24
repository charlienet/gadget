package logger

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- M-2：New 替换默认实例前关闭旧实例（flush 异步队列 + 注册表不累积）---

func TestNewClosesPreviousDefault(t *testing.T) {
	// 保存/恢复包级默认状态，避免污染后续测试
	defaultMu.Lock()
	prevLogger, prevLeveler, prevInst := DefaultLogger, defaultLeveler, defaultInstance
	defaultMu.Unlock()
	t.Cleanup(func() {
		defaultMu.Lock()
		DefaultLogger, defaultLeveler, defaultInstance = prevLogger, prevLeveler, prevInst
		defaultMu.Unlock()
	})

	// 实例 A：异步 + buffer 输出（模拟一个持有未落盘队列的旧默认实例）
	var bufA bytes.Buffer
	lA := New(WithConsole(WithConsoleWriter(&bufA)), WithAsync(64), WithConsole(WithConsoleColor(false)))
	_ = lA
	lA.Info("flushed by replacement")

	// 实例 B：再 New 应同步关闭 A —— 返回时 A 队列必已排空
	lB := New(WithConsole(WithConsoleWriter(io.Discard)), WithConsole(WithConsoleColor(false)))

	gotA := bufA.String()
	if !strings.Contains(gotA, "flushed by replacement") {
		t.Errorf("expected previous default's async queue flushed on replacement, got: %q", gotA)
	}

	// 注册表不累积：A 已 close+注销；注册表内容即 B（不枚举比较，避免依赖其他测试残留实例）
	if got := registrySnapshot(); len(got) != 1 || got[0].slog != lB {
		t.Errorf("expected registry to hold exactly the new default B, got %d entries", len(got))
	}

	// 清理 B（当前 defaultInstance），注册表回到 base
	defaultMu.Lock()
	inst := defaultInstance
	defaultMu.Unlock()
	if inst != nil {
		_ = inst.close(0)
	}
	for _, v := range registrySnapshot() {
		if v.slog == lB {
			t.Error("expected B unregistered after close")
		}
	}
}

// registrySnapshot 返回注册表内容快照（仅测试断言用）
func registrySnapshot() []*slogLogger {
	loggerMu.Lock()
	defer loggerMu.Unlock()
	out := make([]*slogLogger, len(loggerList))
	copy(out, loggerList)
	return out
}

// --- M-5：defaultMu 下并发改写默认引用不 panic / 注册表操作互斥 ---
// （行为级冒烟：并发 New 的 close-previous + register 组合路径，完整 -race
// 验证见外部 TestConcurrentDefaultLifecycle。）

func TestConcurrentNewRegistryChurn(t *testing.T) {
	defaultMu.Lock()
	prevInst := defaultInstance
	prevLeveler := defaultLeveler
	defaultMu.Unlock()
	t.Cleanup(func() {
		defaultMu.Lock()
		defaultLeveler = prevLeveler
		defaultMu.Unlock()
	})
	_ = prevInst

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 10 {
				_ = New(WithConsole(WithConsoleWriter(io.Discard)), WithConsole(WithConsoleColor(false)))
			}
		})
	}
	wg.Wait()

	// 无论并发顺序如何，最终注册表增量至多为 1（最后写入者），
	// 每个被替换的实例都已 close/注销；清掉当前默认实例恢复现场
	defaultMu.Lock()
	inst := defaultInstance
	defaultMu.Unlock()
	if inst != nil {
		_ = inst.close(0)
	}
}

// --- errors.Join：多实例关闭错误全部保留 ---

// blockingWriter 同 lifecycle_test.go：首次 Write 阻塞，由外部 close(hold) 放行。
type blockingWriter struct {
	once    sync.Once
	started chan struct{}
	hold    chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.hold
	return len(p), nil
}

// TestSlogLoggerCloseJoinsAllErrors 验证 slogLogger.close 在 async 超时（返回错误）
// 且 fileCloser 也返回错误时，两者均被 errors.Join 合并，errors.Is 可双双命中。
func TestSlogLoggerCloseJoinsAllErrors(t *testing.T) {
	// blockingWriter 让 AsyncHandler 消费 goroutine 阻塞 → Close 超时 → 返回错误
	bw := &blockingWriter{started: make(chan struct{}), hold: make(chan struct{})}
	h := NewAsyncHandler(slog.NewTextHandler(bw, nil), 4, false)

	// 触发消费：第一条 Write 阻塞
	go func() {
		rec := slog.NewRecord(time.Now(), slog.LevelInfo, "hold", 0)
		_ = h.Handle(context.Background(), rec)
	}()
	<-bw.started // 确认消费 goroutine 已进入 Write 阻塞

	l := &slogLogger{
		opt:        Options{Async: true},
		level:      NewDynamicLevel(slog.LevelInfo),
		slog:       slog.New(h),
		async:      h,
		fileWriter: io.Discard,
		fileCloser: errCloser{}, // Close 永远返回 errors.New("closer boom")
		closeOnce:  sync.Once{},
	}

	err := l.close(20 * time.Millisecond)
	if err == nil {
		t.Fatal("expected non-nil error when both async timeout and fileCloser fail")
	}
	// fileCloser 固定错误（errCloser 来自同包 console_internal_test.go）
	if !errors.Is(err, errCloserError) {
		t.Errorf("errors.Is(err, errCloserError) = false; want true. err=%v", err)
	}
	// async 超时错误也在链中
	if !strings.Contains(err.Error(), "not drained") {
		t.Errorf("expected async timeout error in chain, got: %v", err)
	}
	// 放行消费 goroutine 避免泄漏
	close(bw.hold)
}

// TestPackageCloseJoinsAllErrors 验证包级 Close 在多个 slogLogger 实例均失败时，
// errors.Join 合并的错误链中 errors.Is 可同时命中每个实例的错误。
func TestPackageCloseJoinsAllErrors(t *testing.T) {
	// 保存并恢复默认状态
	defaultMu.Lock()
	prevInst := defaultInstance
	prevLeveler := defaultLeveler
	defaultMu.Unlock()
	t.Cleanup(func() {
		defaultMu.Lock()
		defaultInstance, defaultLeveler = prevInst, prevLeveler
		defaultMu.Unlock()
	})

	// 实例 1：async 超时（blockingWriter → Close 超时）
	bw1 := &blockingWriter{started: make(chan struct{}), hold: make(chan struct{})}
	h1 := NewAsyncHandler(slog.NewTextHandler(bw1, nil), 4, false)
	go func() {
		rec := slog.NewRecord(time.Now(), slog.LevelInfo, "hold1", 0)
		_ = h1.Handle(context.Background(), rec)
	}()
	<-bw1.started

	l1 := &slogLogger{
		opt:        Options{Async: true},
		level:      NewDynamicLevel(slog.LevelInfo),
		slog:       slog.New(h1),
		async:      h1,
		fileWriter: io.Discard,
		fileCloser: errCloser{}, // fileCloser 成功（errCloser 的失败用错误内容匹配，非 errors.Is）
		closeOnce:  sync.Once{},
	}
	registerLogger(l1)
	t.Cleanup(func() { unregisterLogger(l1) })

	// 实例 2：fileCloser 失败（async 正常关闭，fileCloser 失败）
	bw2 := &blockingWriter{started: make(chan struct{}), hold: make(chan struct{})}
	h2 := NewAsyncHandler(slog.NewTextHandler(bw2, nil), 4, false)
	// 立即放行 h2 的消费：async 正常排空
	close(bw2.hold)

	l2 := &slogLogger{
		opt:        Options{Async: true},
		level:      NewDynamicLevel(slog.LevelInfo),
		slog:       slog.New(h2),
		async:      h2,
		fileWriter: io.Discard,
		fileCloser: errCloser{}, // Close 永远返回 errors.New("closer boom")
		closeOnce:  sync.Once{},
	}
	registerLogger(l2)
	t.Cleanup(func() { unregisterLogger(l2) })

	err := Close(20 * time.Millisecond)
	if err == nil {
		t.Fatal("expected non-nil error from package Close with failing instances")
	}
	// 实例 1 的 async 超时错误（l1.close 返回）
	if !strings.Contains(err.Error(), "not drained") {
		t.Errorf("expected async timeout error in chain, got: %v", err)
	}
	// 实例 2 的 fileCloser 固定错误（errors.Is 可命中）
	if !errors.Is(err, errCloserError) {
		t.Errorf("errors.Is(err, errCloserError) = false; want true. err=%v", err)
	}

	// 放行实例 1 的消费 goroutine 避免泄漏
	close(bw1.hold)
}

// --- 文件输出格式（JSON / Text 后端分流，见 newFileHandler / FileFormat 枚举）---

// newTestFileLogger 构造纯文件 sink 的内部实例：不声明 WithConsole → 控制台不装配 →
// 不写 stdout；不注册、不 SetDefault，避免污染包级默认状态；t.Cleanup 关闭文件句柄。
// format 传 FileFormat 枚举（零值 ""/FormatJSON → JSON，FormatText → console 渲染器 NoColor 形态）。
func newTestFileLogger(t *testing.T, path string, format FileFormat) *slogLogger {
	t.Helper()
	opt := Options{Level: slog.LevelInfo}
	WithFile(path, WithFormat(format))(&opt) // 填充 FileOpts，与 New 真实构造路径一致
	l := newSlogLogger(opt)
	t.Cleanup(func() { _ = l.close(0) }) // 释放 lumberjack 句柄
	return l
}

// tempLogPath 在 t.TempDir 下给出日志文件路径（close(0) 已释放句柄，TempDir 自动清理无残留）
func tempLogPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "app.log")
}

// TestFileHandlerTextFormat：WithFormat(FormatText) 时文件落地为自研排序 text handler 输出
// （前置段裸值、msg 原样不加引号、source 与其余 attrs 为 k=v），而非 JSON；并验证内置时间格式 "2006-01-02 15:04:05.000"。
func TestFileHandlerTextFormat(t *testing.T) {
	path := tempLogPath(t)
	l := newTestFileLogger(t, path, FormatText)
	l.slog.Info("text format msg", "k", "v")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	got := strings.TrimSpace(string(data))

	if strings.HasPrefix(got, "{") {
		t.Fatalf("expected custom text handler output, got JSON-looking line: %s", got)
	}
	// 自研 text handler 版式：级别为裸值 INFO（与 console 缩写风格一致）、msg 原样、无 key= 前缀
	if !strings.Contains(got, "INFO") || !strings.Contains(got, "text format msg") {
		t.Errorf("expected bare-value text format, got: %s", got)
	}
	if strings.Contains(got, "level=") || strings.Contains(got, "msg=") {
		t.Errorf("front segment and msg must be bare values, got: %s", got)
	}
	// 内置时间格式：行首裸值 2006-01-02 15:04:05.000（含空格、无 RFC3339 的 T、无 time= 前缀）。
	timeRe := regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3} `)
	if !timeRe.MatchString(got) {
		t.Errorf("expected built-in bare time format at line start, got: %s", got)
	}
}

// TestNewFileHandlerBackendSelection：newFileHandler 按 FileFormat 枚举选择后端——
// FormatText → 自研 text handler；FormatJSON / ""(零值) / 未知枚举 → JSON（default 兜底，向后兼容）。
// 注：原「大小写不敏感/trim」的字符串解析已上移至 ParseFileFormat（见 config_test 的表驱动用例）。
func TestNewFileHandlerBackendSelection(t *testing.T) {
	handlerOpts := &slog.HandlerOptions{Level: slog.LevelInfo}
	cases := []struct {
		name     string
		format   FileFormat
		wantText bool
	}{
		{"text", FormatText, true},
		{"json", FormatJSON, false},
		{"empty-zero-value", "", false},
		{"unknown-fallback", FileFormat("bogus"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			lg := slog.New(newFileHandler(&buf, c.format, handlerOpts))
			lg.Info("pick msg", "k", "v")
			got := strings.TrimSpace(buf.String())
			if c.wantText {
				if strings.HasPrefix(got, "{") || !strings.Contains(got, "INFO") || strings.Contains(got, "level=") {
					t.Errorf("want text handler (format %q), got: %s", c.format, got)
				}
			} else if !strings.HasPrefix(got, "{") || !strings.Contains(got, `"level":"INFO"`) {
				t.Errorf("want JSON handler (format %q), got: %s", c.format, got)
			}
		})
	}
}

// TestFileHandlerDefaultIsJSON：未设 FileFormat（空串）时文件仍为 JSON，保证向后兼容。
func TestFileHandlerDefaultIsJSON(t *testing.T) {
	path := tempLogPath(t)
	l := newTestFileLogger(t, path, "") // 空格式 → 默认 JSON
	l.slog.Info("json compat msg", "k", "v")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	got := strings.TrimSpace(string(data))

	if !strings.HasPrefix(got, "{") {
		t.Fatalf("expected JSON object by default, got: %s", got)
	}
	if !strings.Contains(got, `"level":"INFO"`) || !strings.Contains(got, `"msg":"json compat msg"`) {
		t.Errorf("expected JSON fields, got: %s", got)
	}
	// 时间定制对 JSON 同样保留（与既有行为一致）
	if !regexp.MustCompile(`"time":"[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}"`).MatchString(got) {
		t.Errorf("expected custom time format in JSON, got: %s", got)
	}
}
