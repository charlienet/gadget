package logger_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlienet/gadget/logger"
)

// --- M-3：AsyncHandler.Close 超时返回携带残余数的错误 ---

// blockingWriter 首次 Write 阻塞至 hold 关闭；started 在首次进入时信号一次。
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

func TestAsyncCloseTimeoutError(t *testing.T) {
	bw := &blockingWriter{started: make(chan struct{}), hold: make(chan struct{})}
	h := logger.NewAsyncHandler(slog.NewTextHandler(bw, nil), 8, false)

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "pending", 0)
	// 第一条被消费 goroutine 取走并阻塞在 Write；后两条滞留队列
	for range 3 {
		if err := h.Handle(context.Background(), rec); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	<-bw.started // 确认消费 goroutine 已持有第一条，剩余恰为 2 条残余

	err := h.Close(20 * time.Millisecond)
	if err == nil {
		t.Fatal("expected Close to return error when queue cannot drain in time")
	}
	if !strings.Contains(err.Error(), "not drained") || !strings.Contains(err.Error(), "pending") {
		t.Errorf("expected descriptive timeout error, got: %v", err)
	}
	// N-4：残余计数精确断言（文案 "2 records still pending"；此前断言 "2" 会被
	// 超时值 "20ms" 误满足，形同虚设）
	if !strings.Contains(err.Error(), "2 records still pending") {
		t.Errorf("expected residual count 2 in error, got: %v", err)
	}

	// 放行消费：残余最终落盘；重复 Close 命中已关闭分支返回 nil
	close(bw.hold)
	if err := h.Close(2 * time.Second); err != nil {
		t.Errorf("second Close (already closed) must return nil, got: %v", err)
	}
}

// 正常排空路径必须仍返回 nil（含经包级 Close 聚合的场景）
func TestAsyncCloseDrainedReturnsNil(t *testing.T) {
	var buf strings.Builder
	h := logger.NewAsyncHandler(slog.NewTextHandler(&buf, nil), 8, false)
	h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "drained", 0))
	if err := h.Close(2 * time.Second); err != nil {
		t.Errorf("expected nil on clean drain, got: %v", err)
	}
	if !strings.Contains(buf.String(), "drained") {
		t.Errorf("expected record written, got: %q", buf.String())
	}
}

// --- M-5：并发 Init / SetLevel / Fatal（ExitFunc 注入防退出）——-race 须干净 ---

func TestConcurrentDefaultLifecycle(t *testing.T) {
	withRestoreDefault(t)
	t.Cleanup(func() { logger.Close(5 * time.Second) })

	// 先把 DefaultLogger 指到 discard，避免并发首轮 Init 之前 Fatal 打到真实 stdout
	logger.DefaultLogger = logger.New(logger.WithOutput(io.Discard), logger.WithColor(false))

	origExit := logger.ExitFunc
	logger.ExitFunc = func(int) {}
	defer func() { logger.ExitFunc = origExit }()

	// Init 默认输出指向 os.Stdout：临时改到 /dev/null，避免并发日志噪音与管道阻塞
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	origOut := os.Stdout
	os.Stdout = devNull
	defer func() { os.Stdout = origOut; devNull.Close() }()

	var wg sync.WaitGroup

	// 并发 Init：写 DefaultLogger / defaultInstance / defaultLeveler + 关闭旧默认实例
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				if err := logger.Init(logger.Config{
					Level: "info", Output: "console", Async: true, QueueSize: 64,
				}); err != nil {
					t.Errorf("Init: %v", err)
					return
				}
			}
		}()
	}

	// 并发 SetLevel：锁下读取 defaultLeveler 后原子调整
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				logger.SetLevel(logger.Warn)
				logger.SetLevel(logger.Debug)
			}
		}()
	}

	// 并发 Fatal：锁下捕获 DefaultLogger/defaultInstance 引用 → Log → 尽力 flush
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				logger.Fatal("concurrent fatal check", "g", "x")
			}
		}()
	}

	wg.Wait()

	// 存活断言：默认引用仍可用（未因关闭顺序 panic/死锁）
	logger.DefaultLogger.Info("still alive")
}

// --- M-6：file/both + 空 File 的黑洞配置返回错误且不改动默认 logger ---

func TestInitBlackholeConfigRejected(t *testing.T) {
	orig := logger.DefaultLogger
	t.Cleanup(func() {
		logger.DefaultLogger = orig
		slog.SetDefault(orig)
	})

	for _, out := range []string{"file", "both"} {
		err := logger.Init(logger.Config{Output: out, File: ""})
		if err == nil {
			t.Errorf("Init(output=%q, file=\"\") must return error", out)
			continue
		}
		if !strings.Contains(err.Error(), "requires non-empty file path") {
			t.Errorf("unexpected error for output=%q: %v", out, err)
		}
		// 失败不得替换 DefaultLogger
		if logger.DefaultLogger != orig {
			t.Errorf("DefaultLogger must stay untouched on rejected Init (output=%q)", out)
		}
	}

	// console + 空 File 维持现状：不报错
	if err := logger.Init(logger.Config{Output: "console", File: ""}); err != nil {
		t.Errorf("Init(console, no file) should succeed, got: %v", err)
	}
	// file + 有 File：正常
	dir := t.TempDir()
	if err := logger.Init(logger.Config{Output: "file", File: dir + "/ok.log"}); err != nil {
		t.Errorf("Init(file with path) should succeed, got: %v", err)
	}
}
