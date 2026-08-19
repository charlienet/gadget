package logger_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlienet/gadget/logger"
)

// 异步测试统一约定：
//   - t.Cleanup 调用包级 logger.Close，确保队列关闭，避免 goroutine 泄漏影响其他测试；
//     Close 幂等，多测试重复调用安全。
//   - 不用 Fatal（会触发 ExitFunc/Close 副作用），统一用 t.Error。

func TestAsyncFlush(t *testing.T) {
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithAsync(), logger.WithColor(false))

	l.Info("async msg")
	logger.Close(2 * time.Second) // flush 队列，队列内日志落盘

	if !strings.Contains(buf.String(), "async msg") {
		t.Errorf("expected async msg flushed to output, got: %q", buf.String())
	}
}

func TestAsyncNonBlocking(t *testing.T) {
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	var buf bytes.Buffer
	// 8 容量的极小队列：阻塞模式会卡住并发调用方，非阻塞必须快速返回
	l := logger.New(logger.WithOutput(&buf), logger.WithAsync(8), logger.WithColor(false))

	var wg sync.WaitGroup
	for range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Info("nonblocking msg")
		}()
	}

	// 断言 goroutine 全部完成（非阻塞生效）；若阻塞则 5s 超时报错
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("async logging blocked the caller (queue too small)")
	}

	// 统计须在包级 Close 前读取（Close 会清空注册表）；dropped 可能有，不断言
	total, _ := logger.Stats()
	if total != 1000 {
		t.Errorf("expected total=1000, got %d", total)
	}
}

func TestAsyncBlocking(t *testing.T) {
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	var buf bytes.Buffer
	// 队列容量 1 + 阻塞模式：并发写触发队列满时的阻塞背压路径，但不丢日志
	l := logger.New(logger.WithOutput(&buf), logger.WithAsync(1), logger.WithAsyncBlocking(), logger.WithColor(false))

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Info("blocking msg")
		}()
	}
	wg.Wait() // 阻塞模式下队列满时等待消费，最终全部入队
	logger.Close(2 * time.Second)

	// 阻塞模式绝不丢：10 条全部落盘
	if got := strings.Count(buf.String(), "blocking msg"); got != 10 {
		t.Errorf("expected 10 blocking msg in output, got %d: %q", got, buf.String())
	}
}

func TestAsyncPreserveSource(t *testing.T) {
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	var buf bytes.Buffer
	// WithSource(true)：异步复制 Record 保留 PC，源码位置不丢失
	l := logger.New(logger.WithOutput(&buf), logger.WithAsync(), logger.WithSource(true), logger.WithColor(false))

	l.Info("src")
	logger.Close(2 * time.Second)

	if !strings.Contains(buf.String(), "source=") {
		t.Errorf("expected source= in output (PC preserved through async), got: %q", buf.String())
	}
}

func TestAsyncWithAttrs(t *testing.T) {
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithAsync(), logger.WithColor(false))

	l.With("k", "v").Info("attrs msg")
	logger.Close(2 * time.Second)

	if !strings.Contains(buf.String(), "k=v") {
		t.Errorf("expected k=v in output (attrs preserved through async), got: %q", buf.String())
	}
}

// TestAsyncCloseConcurrent 验证 S1 竞态修复：并发写日志的同时反复包级 Close + SetOutput，
// 不得出现 send on closed channel panic（-race 下运行）。
// 注意：包级 Close 会清空注册表，每轮需重新创建 logger；t.Cleanup 兜底。
func TestAsyncCloseConcurrent(t *testing.T) {
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	var buf1, buf2 bytes.Buffer
	var wg sync.WaitGroup

	for range 5 {
		l := logger.New(logger.WithOutput(&buf1), logger.WithAsync(), logger.WithColor(false))

		stop := make(chan struct{})
		// 持续写日志的 goroutine：Close 后 Handle 应静默丢弃而非 panic
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						l.Info("concurrent msg")
					}
				}
			}()
		}

		// 反复包级 Close（关闭注册的 async）+ SetOutput（热切换 writer）的 goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				logger.Close(100 * time.Millisecond)
				l.SetOutput(&buf2)
				l.Info("after close")
			}
			close(stop)
		}()

		wg.Wait()
	}
}

// TestSetOutputShared 验证 S4 修复：SetOutput 热切换 writer 指针，
// 派生实例（With）共享 outPtr，SetOutput 后派生实例输出到新目标。
func TestSetOutputShared(t *testing.T) {
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	var buf1, buf2 bytes.Buffer
	base := logger.New(logger.WithOutput(&buf1), logger.WithAsync(), logger.WithColor(false))
	derived := base.With("k", "v")

	base.SetOutput(&buf2) // 热切换：派生实例共享 outPtr，全局生效
	derived.Info("msg")

	logger.Close(2 * time.Second) // flush 异步队列

	if !strings.Contains(buf2.String(), "msg") {
		t.Errorf("expected msg in new output (SetOutput shared by derived), got: %q", buf2.String())
	}
	if !strings.Contains(buf2.String(), "k=v") {
		t.Errorf("expected derived attrs preserved, got: %q", buf2.String())
	}
}

// TestInstanceClose 验证 A4：实例 Close() 关闭自己的 async（flush 队列）
// 并从包级注册表注销，包级 Stats 不再统计该实例。
func TestInstanceClose(t *testing.T) {
	t.Cleanup(func() { logger.Close(2 * time.Second) })

	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithAsync(), logger.WithColor(false))
	l.Info("instance close msg")

	c, ok := l.(interface{ Close() error })
	if !ok {
		t.Fatal("expected slog logger to expose Close()")
	}
	if err := c.Close(); err != nil {
		t.Errorf("instance Close failed: %v", err)
	}

	if !strings.Contains(buf.String(), "instance close msg") {
		t.Errorf("expected async msg flushed on instance Close, got: %q", buf.String())
	}

	// 实例 Close 后从包级注册表注销：Stats 不应再统计该实例的 async
	total, _ := logger.Stats()
	if total != 0 {
		t.Errorf("expected 0 stats after instance Close, got %d", total)
	}
}
