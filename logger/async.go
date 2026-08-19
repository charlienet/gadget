package logger

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncHandler 异步日志处理器：非阻塞入队 + 后台消费 goroutine。
// 相比参考实现修正的缺陷：
//  1. 复制 Record 时保留 r.PC（异步模式下 AddSource 源码位置不丢失）
//  2. WithAttrs/WithGroup 派生实例共享同一队列与关闭状态（不会出现关闭后向已关闭 channel 发送导致 panic）
//  3. 预设属性不丢失：WithAttrs/WithGroup 透传底层 handler，由底层在 Handle 时合并
type AsyncHandler struct {
	handler slog.Handler // 底层实际处理器（负责格式化输出）
	state   *asyncState  // 共享状态：队列、关闭标志、统计
}

// asyncItem 入队单元：Record 复制品 + 当前实例的 handler。
// handler 随 record 一起入队，保证 WithAttrs/WithGroup 派生的预设属性/分组
// 由各自的底层 handler 在消费时合并（process 启动时捕获的原始 handler 不含派生 attrs）。
type asyncItem struct {
	handler slog.Handler
	rec     slog.Record
}

// asyncState 异步处理器共享状态：队列、关闭标志、统计
type asyncState struct {
	// mu 保护「closed 检查 + 入队 + close(ch)」三者互斥，消除 send-on-closed-channel 竞态。
	// 无竞争时 mutex 开销可忽略（一条日志一次 Lock/Unlock），换来并发正确性。
	mu       sync.Mutex
	ch       chan asyncItem // 队列（存 Record 复制品 + 派生 handler）
	done     chan struct{}  // 消费完成信号
	closed   atomic.Bool
	blocking bool // 队列满时是否阻塞（true=背压绝不丢；false=丢弃并计数）
	dropped  atomic.Uint64
	total    atomic.Uint64
}

// NewAsyncHandler 创建异步处理器：
// queueSize <= 0 时默认 10240；启动后台 process goroutine 消费队列。
func NewAsyncHandler(handler slog.Handler, queueSize int, blocking bool) *AsyncHandler {
	if queueSize <= 0 {
		queueSize = 10240
	}

	h := &AsyncHandler{
		handler: handler,
		state: &asyncState{
			ch:       make(chan asyncItem, queueSize),
			done:     make(chan struct{}),
			blocking: blocking,
		},
	}
	go h.process()

	return h
}

// Enabled 透传底层 handler 的级别判断（slog 级别数值越小越详细）
func (h *AsyncHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

// Handle 复制 Record 入队后立即返回（不落盘，不阻塞主业务）。
// 检查+入队在 state.mu 锁内完成：与 Close 的 close(ch) 互斥，
// 保证「closed 检查通过时 channel 必然未关闭」，杜绝 send on closed channel panic。
func (h *AsyncHandler) Handle(_ context.Context, r slog.Record) error {
	// 复制 Record：保留 PC（源码位置），重新收集 attrs
	rec := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		rec.AddAttrs(a)
		return true
	})

	h.state.mu.Lock()
	if h.state.closed.Load() {
		h.state.mu.Unlock()
		return nil // 已关闭，静默丢弃
	}
	h.state.total.Add(1)

	if h.state.blocking {
		select {
		case h.state.ch <- asyncItem{handler: h.handler, rec: rec}:
		case <-h.state.done: // 防御分支（锁内 closed 检查已保证不会走到）
		}
		h.state.mu.Unlock()
		return nil
	}

	// 非阻塞：队列满则丢弃并计数（绝不卡主业务）
	select {
	case h.state.ch <- asyncItem{handler: h.handler, rec: rec}:
	default:
		h.state.dropped.Add(1)
	}
	h.state.mu.Unlock()
	return nil
}

// WithAttrs 派生实例共享同一队列与关闭状态；预设属性透传底层 handler
func (h *AsyncHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &AsyncHandler{handler: h.handler.WithAttrs(attrs), state: h.state}
}

// WithGroup 派生实例共享同一队列与关闭状态；分组透传底层 handler
func (h *AsyncHandler) WithGroup(name string) slog.Handler {
	return &AsyncHandler{handler: h.handler.WithGroup(name), state: h.state}
}

// process 后台消费 goroutine：从队列取 asyncItem，用 item 自带的 handler 落盘，
// 退出时发消费完成信号
func (h *AsyncHandler) process() {
	for item := range h.state.ch {
		_ = item.handler.Handle(context.Background(), item.rec)
	}
	close(h.state.done)
}

// Close 幂等关闭：锁内检查/置位 + close(ch)，放锁后才等待消费完成（等待期间不持锁）。
// timeout <= 0 时默认 2s；超时强制返回。
func (h *AsyncHandler) Close(timeout time.Duration) error {
	// 锁内置位 + close(ch)：与 Handle 的「closed 检查 + 入队」互斥，
	// 锁内 send 到已关闭 channel 不可能发生（检查与 close 同锁互斥）。
	h.state.mu.Lock()
	if h.state.closed.Swap(true) {
		h.state.mu.Unlock()
		return nil // 已关闭
	}
	close(h.state.ch)
	h.state.mu.Unlock()

	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	select {
	case <-h.state.done:
	case <-time.After(timeout): // 超时强制返回
	}
	return nil
}

// Stats 返回累计统计：total=写入总数，dropped=因队列满被丢弃数
func (h *AsyncHandler) Stats() (total, dropped uint64) {
	return h.state.total.Load(), h.state.dropped.Load()
}

// ---- 包级 logger 实例注册表 ----

var (
	loggerMu   sync.Mutex
	loggerList []*slogLogger
)

// registerLogger 注册 slogLogger 实例（newSlogLogger 创建时调用）。
// 实例持有 async 处理器与文件 writer，由包级 Close 或实例 Close() 统一释放。
func registerLogger(l *slogLogger) {
	loggerMu.Lock()
	defer loggerMu.Unlock()
	loggerList = append(loggerList, l)
}

// unregisterLogger 从包级注册表注销实例（实例 Close() 时调用，幂等）
func unregisterLogger(l *slogLogger) {
	loggerMu.Lock()
	defer loggerMu.Unlock()
	for i, v := range loggerList {
		if v == l {
			loggerList = append(loggerList[:i], loggerList[i+1:]...)
			return
		}
	}
}

// ---- 包级文件 writer 注册表 ----
// 仅 recorder 路径（options.go WithRecorder）注册的文件 writer 需要包级 Close 兜底；
// slog 路径的文件句柄由各自实例 Close() 释放（见 slogLogger.close）。

var (
	fileCloseMu     sync.Mutex
	fileClosersList []io.Closer
)

// registerFileCloser 注册文件 writer 到包级注册表（recorder 路径创建时调用）
func registerFileCloser(c io.Closer) {
	fileCloseMu.Lock()
	defer fileCloseMu.Unlock()
	fileClosersList = append(fileClosersList, c)
}

// Stats 返回所有已注册 logger 实例的异步累计统计（total, dropped）。
// 注意：包级 Close 会清空注册表，如需观测请在 Close 前调用。
func Stats() (total, dropped uint64) {
	loggerMu.Lock()
	ls := make([]*slogLogger, len(loggerList))
	copy(ls, loggerList)
	loggerMu.Unlock()

	for _, l := range ls {
		if a := l.async; a != nil {
			t, d := a.Stats()
			total += t
			dropped += d
		}
	}
	return
}

// Close 优雅关闭所有已注册 logger 实例（进程退出前调用）：
// 关闭各实例的异步处理器（确保队列日志落盘）与文件 writer，并关闭 recorder 路径注册的文件 writer。
// 幂等：重复调用安全。
// 注意：异步 logger 建议进程退出前调用包级 logger.Close()，或对关键实例调用其 Close() 方法。
// 用 copy 快照遍历，避免持锁时 Close 内部等待；重复调用第二次注册表已空，安全。
func Close(timeout time.Duration) error {
	loggerMu.Lock()
	ls := make([]*slogLogger, len(loggerList))
	copy(ls, loggerList)
	loggerList = nil
	loggerMu.Unlock()

	fileCloseMu.Lock()
	fcs := make([]io.Closer, len(fileClosersList))
	copy(fcs, fileClosersList)
	fileClosersList = nil
	fileCloseMu.Unlock()

	var err error
	for _, l := range ls {
		if e := l.close(timeout); e != nil {
			err = e
		}
	}
	for _, c := range fcs {
		if e := c.Close(); e != nil {
			err = e
		}
	}
	return err
}
