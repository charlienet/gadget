package logger

import (
	"context"
	"encoding"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ConsoleOptions 控制台 handler 配置
type ConsoleOptions struct {
	Level     slog.Leveler // 日志级别（传 *DynamicLevel 即动态可调）
	AddSource bool         // 源码位置
	NoColor   bool         // 禁用颜色
}

// ANSI 亮色
const (
	colorTrace     = "\033[90m"   // 亮灰
	colorDebug     = "\033[94m"   // 亮蓝
	colorInfo      = "\033[96m"   // 亮青
	colorWarn      = "\033[93m"   // 亮黄
	colorError     = "\033[91m"   // 亮红
	colorFatal     = "\033[1;91m" // 亮红加粗
	colorKey       = "\033[94m"   // 亮蓝
	colorTimestamp = "\033[97m"   // 亮白
	colorReset     = "\033[0m"
)

// consoleHandler 彩色控制台 slog.Handler
// 注意：mu 用指针共享，WithAttrs/WithGroup 派生时值拷贝安全
type consoleHandler struct {
	mu     *sync.Mutex
	w      *atomic.Pointer[io.Writer] // 共享 writer 引用：SetOutput 热切换（原子换指针，不重建链）
	opts   ConsoleOptions
	attrs  []slog.Attr
	groups []string
}

func NewConsoleHandler(w io.Writer, opts *ConsoleOptions) slog.Handler {
	// 创建独立共享指针：NewConsoleHandler 的使用者自行 SetOutput 时换掉指针内容即可
	p := &atomic.Pointer[io.Writer]{}
	p.Store(&w)
	return newConsoleHandlerWithPtr(p, opts)
}

// newConsoleHandlerWithPtr 使用外部传入的共享 writer 指针构造 handler
// （default.go 的 slogLogger 持有同一 outPtr，SetOutput 全局生效于所有派生实例）
func newConsoleHandlerWithPtr(p *atomic.Pointer[io.Writer], opts *ConsoleOptions) slog.Handler {
	return &consoleHandler{mu: &sync.Mutex{}, w: p, opts: *opts}
}

// Enabled 判断级别是否启用（slog 级别数值越小越详细）
func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.opts.Level.Level()
}

// bufPool 控制台输出缓冲池：避免每条日志 make 堆分配 []byte。
// Put 时截断到 maxPooledBufSize 上限，防止超大缓冲（单条超长日志）滞留池中。
var bufPool = sync.Pool{
	New: func() any { return make([]byte, 0, 256) },
}

// maxPooledBufSize 允许放回池中的缓冲容量上限（超出则丢弃，交由 GC 回收）
const maxPooledBufSize = 4096

// Handle 拼装完整行后一次性写入（整行加锁，避免并发写交错）
func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	buf := bufPool.Get().([]byte)[:0]
	defer func() {
		if cap(buf) <= maxPooledBufSize {
			bufPool.Put(buf[:0])
		}
	}()

	// 时间戳（彩色），格式 "2006-01-02 15:04:05.000"
	if !r.Time.IsZero() {
		if !h.opts.NoColor {
			buf = append(buf, colorTimestamp...)
		}
		buf = append(buf, r.Time.Format("2006-01-02 15:04:05.000")...)
		if !h.opts.NoColor {
			buf = append(buf, colorReset...)
		}
	}

	// 级别（彩色）
	buf = append(buf, ' ')
	if !h.opts.NoColor {
		buf = append(buf, levelColor(r.Level)...)
	}
	buf = append(buf, '[')
	buf = append(buf, formatLevel(r.Level)...)
	buf = append(buf, ']')
	if !h.opts.NoColor {
		buf = append(buf, colorReset...)
	}

	// 消息本体不上色
	buf = append(buf, ' ')
	buf = append(buf, r.Message...)

	// 源码位置
	if h.opts.AddSource && r.PC != 0 {
		if src, ok := sourceFromPC(r.PC); ok {
			buf = append(buf, " source="...)
			buf = append(buf, src...)
		}
	}

	// 属性：先输出 WithAttrs 累积的 h.attrs，再输出记录自身 attrs，均带分组前缀
	prefix := h.groupPrefix()
	buf = h.appendAttrs(buf, h.attrs, prefix)
	r.Attrs(func(a slog.Attr) bool {
		buf = h.appendAttr(buf, a, prefix)
		return true
	})

	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()

	// 原子加载当前 writer（SetOutput 热切换后新写入落到新目标）
	w := h.w.Load()
	_, err := (*w).Write(buf)
	return err
}

// WithAttrs 返回追加属性的新实例（值拷贝，共享 w/opts）
func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	h2 := *h
	h2.attrs = make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	h2.attrs = append(h2.attrs, h.attrs...)
	h2.attrs = append(h2.attrs, attrs...)

	return &h2
}

// WithGroup 返回带分组前缀的新实例
func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	h2 := *h
	h2.groups = make([]string, 0, len(h.groups)+1)
	h2.groups = append(h2.groups, h.groups...)
	h2.groups = append(h2.groups, name)

	return &h2
}

// groupPrefix 返回当前分组前缀（如 "g.h."）
func (h *consoleHandler) groupPrefix() string {
	if len(h.groups) == 0 {
		return ""
	}

	return strings.Join(h.groups, ".") + "."
}

// appendAttrs 批量输出属性
func (h *consoleHandler) appendAttrs(buf []byte, attrs []slog.Attr, prefix string) []byte {
	for _, a := range attrs {
		buf = h.appendAttr(buf, a, prefix)
	}

	return buf
}

// appendAttr 输出单个属性，Group 类型递归展开
func (h *consoleHandler) appendAttr(buf []byte, a slog.Attr, prefix string) []byte {
	if a.Value.Kind() == slog.KindGroup {
		inner := a.Value.Group()
		next := prefix + a.Key + "."
		for _, ga := range inner {
			buf = h.appendAttr(buf, ga, next)
		}

		return buf
	}

	key := prefix + a.Key

	if !h.opts.NoColor {
		buf = append(buf, colorKey...)
	}
	buf = append(buf, ' ')
	buf = append(buf, key...)
	buf = append(buf, '=')
	if !h.opts.NoColor {
		buf = append(buf, colorReset...)
	}

	return appendValue(buf, a.Value)
}

// formatLevel 级别缩写（4 字符）
func formatLevel(level slog.Level) string {
	switch {
	case level <= Trace:
		return "TRAC"
	case level <= Debug:
		return "DEBU"
	case level <= Info:
		return "INFO"
	case level <= Warn:
		return "WARN"
	case level <= Error:
		return "ERRO"
	default:
		return "FATA"
	}
}

// levelColor 级别对应颜色
func levelColor(level slog.Level) string {
	switch {
	case level <= Trace:
		return colorTrace
	case level <= Debug:
		return colorDebug
	case level <= Info:
		return colorInfo
	case level <= Warn:
		return colorWarn
	case level <= Error:
		return colorError
	default:
		return colorFatal
	}
}

// appendValue 输出 slog.Value（支持全部 Kind）
func appendValue(buf []byte, v slog.Value) []byte {
	switch v.Kind() {
	case slog.KindString:
		return append(buf, v.String()...)
	case slog.KindInt64:
		return strconv.AppendInt(buf, v.Int64(), 10)
	case slog.KindUint64:
		return strconv.AppendUint(buf, v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.AppendFloat(buf, v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.AppendBool(buf, v.Bool())
	case slog.KindDuration:
		return append(buf, v.Duration().String()...)
	case slog.KindTime:
		return append(buf, v.Time().Format("2006-01-02 15:04:05.000")...)
	case slog.KindAny:
		return appendAny(buf, v.Any())
	default:
		// KindGroup 理论上已在 appendAttr 展开，此处防御性兜底
		g := v.Group()
		buf = append(buf, '{')
		for i, a := range g {
			if i > 0 {
				buf = append(buf, ' ')
			}
			buf = append(buf, a.Key...)
			buf = append(buf, '=')
			buf = appendValue(buf, a.Value)
		}

		return append(buf, '}')
	}
}

// appendAny 输出 Any 值：优先 encoding.TextMarshaler，否则 %+v
func appendAny(buf []byte, v any) []byte {
	if tm, ok := v.(encoding.TextMarshaler); ok {
		if b, err := tm.MarshalText(); err == nil {
			return append(buf, b...)
		}
	}

	return append(buf, fmt.Sprintf("%+v", v)...)
}

// sourceFromPC 从调用栈 PC 解析 file:line
func sourceFromPC(pc uintptr) (string, bool) {
	fs := runtime.CallersFrames([]uintptr{pc})
	f, _ := fs.Next()
	if f.File == "" {
		return "", false
	}

	return fmt.Sprintf("%s:%d", f.File, f.Line), true
}
