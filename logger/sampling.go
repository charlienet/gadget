package logger

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// SamplingOptions 采样配置
type SamplingOptions struct {
	First      int           // 每个消息（级别+内容）前 N 条保留
	Thereafter int           // 之后每 M 条保留 1 条
	Window     time.Duration // 统计窗口（<=0 时默认 5s）
}

// samplerCounter 单个消息（级别+内容）的采样计数
type samplerCounter struct {
	count int
	start time.Time
}

// samplingHandler 日志采样装饰器：窗口内按（级别+消息）计数，
// 超过 First 后每 Thereafter 条保留 1 条。
// 注意：mu 用指针共享，WithAttrs/WithGroup 派生时与父实例共享计数与锁
// （采样按全局消息流统计，不因派生拆组）。
// 采样对动态消息（含缓存 key/请求 ID 等每次不同的消息文本）无效——
// 每条消息都作为独立 key 计数，永远在 First 阈值内被保留。
// 建议采样与固定消息+attr 写法搭配：msg 恒定，动态信息放结构化属性。
type samplingHandler struct {
	handler    slog.Handler
	first      int
	thereafter int
	window     time.Duration
	mu         *sync.Mutex
	counters   map[string]*samplerCounter
	calls      int // 调用计数：每 scanInterval 次扫描清理过期 counter（防止无界增长）
}

// scanInterval 触发过期 counter 清理的调用间隔
const scanInterval = 1024

// NewSamplingHandler 采样装饰器：窗口内按（级别+消息）计数，超过 First 后
// 每 Thereafter 保留 1 条。window <= 0 时默认 5s；thereafter <= 0 时视为 1（不额外丢弃）。
func NewSamplingHandler(handler slog.Handler, opts *SamplingOptions) slog.Handler {
	h := &samplingHandler{
		handler:  handler,
		window:   5 * time.Second,
		mu:       &sync.Mutex{},
		counters: make(map[string]*samplerCounter),
	}
	if opts != nil {
		h.first = opts.First
		h.thereafter = opts.Thereafter
		if opts.Window > 0 {
			h.window = opts.Window
		}
	}
	if h.first < 0 {
		h.first = 0
	}
	if h.thereafter <= 0 {
		h.thereafter = 1
	}

	return h
}

// Enabled 透传底层 handler 的级别判断（slog 级别数值越小越详细）
func (h *samplingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

// Handle 窗口内按（级别+消息）计数：n <= First 或 (n-First)%Thereafter==0 时透传底层，否则丢弃。
// 单互斥锁保护 counters；窗口过期或首次出现时重建 counter；
// 每 scanInterval 次调用扫描一次 counters，删除窗口已过期的项（动态消息长期运行不再缓慢泄漏）。
func (h *samplingHandler) Handle(ctx context.Context, r slog.Record) error {
	key := r.Level.String() + "|" + r.Message

	now := time.Now()
	h.mu.Lock()
	h.calls++
	if h.calls%scanInterval == 0 {
		h.gcCounters(now)
	}
	c, ok := h.counters[key]
	if !ok || now.Sub(c.start) >= h.window {
		c = &samplerCounter{start: now}
		h.counters[key] = c
	}
	c.count++
	n := c.count
	h.mu.Unlock()

	if n <= h.first {
		return h.handler.Handle(ctx, r)
	}
	if (n-h.first)%h.thereafter == 0 {
		return h.handler.Handle(ctx, r)
	}

	return nil // 采样丢弃
}

// gcCounters 清理窗口已过期的 counter（调用方须持有 h.mu）
func (h *samplingHandler) gcCounters(now time.Time) {
	for k, c := range h.counters {
		if now.Sub(c.start) >= h.window {
			delete(h.counters, k)
		}
	}
}

// WithAttrs 派生实例：共享计数与锁（同父实例采样统计）
func (h *samplingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	return &samplingHandler{
		handler:    h.handler.WithAttrs(attrs),
		first:      h.first,
		thereafter: h.thereafter,
		window:     h.window,
		mu:         h.mu,
		counters:   h.counters,
	}
}

// WithGroup 派生实例：共享计数与锁（同 WithAttrs）
func (h *samplingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	return &samplingHandler{
		handler:    h.handler.WithGroup(name),
		first:      h.first,
		thereafter: h.thereafter,
		window:     h.window,
		mu:         h.mu,
		counters:   h.counters,
	}
}
