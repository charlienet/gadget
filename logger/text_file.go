package logger

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// FileTextOptions 文件 text handler 配置。
// 时间格式内置（"2006-01-02 15:04:05.000"），不对外暴露，与 console 风格统一。
type FileTextOptions struct {
	Level     slog.Leveler // 日志级别（传 *DynamicLevel 即动态可调）
	AddSource bool         // 源码位置
}

// fileTextHandler 自研文件 text slog.Handler。
//
// 与标准 slog.NewTextHandler 的区别：标准实现字段顺序固定为
// time/level/msg/source/attrs，trace_id 落在末尾不满足需求。本 handler 精确控制
// 字段顺序为：time / level / trace_id(如有) / req_id(如有) / msg / source(可选) /
// 其余 attrs。整行无颜色、空格分隔、k=v 风格。
//
// mu 用指针共享，WithAttrs/WithGroup 派生时值拷贝安全；w 构造后固定
// （与 consoleHandler 同构）。
type fileTextHandler struct {
	mu     *sync.Mutex
	w      io.Writer
	opts   FileTextOptions
	attrs  []slog.Attr // WithAttrs 累积的属性（输出时置于 record 自身 attrs 之前）
	groups []string    // WithGroup 累积的分组前缀
}

// newFileTextHandler 构造文件 text handler：w 为输出目标（如文件 writer），
// opts 为值拷贝，派生实例共享同一 writer 与互斥锁。
// opts 或其 Level 为 nil 时回退默认（Level=Info、不记 source）。
func newFileTextHandler(w io.Writer, opts *FileTextOptions) slog.Handler {
	o := FileTextOptions{}
	if opts != nil {
		o = *opts
	}
	if o.Level == nil {
		o.Level = slog.LevelInfo
	}

	return &fileTextHandler{mu: &sync.Mutex{}, w: w, opts: o}
}

// Enabled 判断级别是否启用（slog 级别数值越小越详细）
func (h *fileTextHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.opts.Level.Level()
}

// Handle 按固定字段顺序拼装完整行后一次性写入（整行加锁，避免并发写交错）。
// 复用 console.go 的 bufPool / maxPooledBufSize 缓冲池与写锁模式。
func (h *fileTextHandler) Handle(_ context.Context, r slog.Record) error {
	buf := bufPool.Get().([]byte)[:0]
	defer func() {
		if cap(buf) <= maxPooledBufSize {
			bufPool.Put(buf[:0]) //nolint:staticcheck // sync.Pool 存取 []byte 为已知误报，boxing 开销远小于改 *[]byte 的重构风险
		}
	}()

	// sep 在字段间补单个空格；首个字段（time 可能零值被跳过）不留行首空格
	sep := func() {
		if len(buf) > 0 {
			buf = append(buf, ' ')
		}
	}

	// 1) 时间：格式内置，r.Time 零值则跳过
	if !r.Time.IsZero() {
		sep()
		buf = append(buf, "time="...)
		buf = append(buf, r.Time.Format("2006-01-02 15:04:05.000")...)
	}

	// 2) 级别：复用 formatLevel（TRAC/DEBU/INFO/WARN/ERRO/FATA），与 console 风格统一
	sep()
	buf = append(buf, "level="...)
	buf = append(buf, formatLevel(r.Level)...)

	// 3) trace_id / req_id：TraceHandler 以 r.AddAttrs 追加为 record 顶层普通属性，
	//    先扫描出这两个属性并前置到 msg 之前；命中后在「其余 attrs」中剔除避免重复。
	//    仅匹配顶层精确 key（非 Group 内、Kind 为 String）；不存在则不输出（即“如有”）。
	//    挑选/去重判据见 record_fields.go，与 consoleHandler 共享同一顺序语义。
	traceID, reqID := scanTraceIDs(r)
	if traceID != "" {
		sep()
		buf = append(buf, AttrTraceID...)
		buf = append(buf, '=')
		buf = appendQuoted(buf, traceID)
	}
	if reqID != "" {
		sep()
		buf = append(buf, AttrReqID...)
		buf = append(buf, '=')
		buf = appendQuoted(buf, reqID)
	}

	// 4) 消息本体
	sep()
	buf = append(buf, "msg="...)
	buf = appendQuoted(buf, r.Message)

	// 5) 源码位置：仅当 AddSource 且 r.PC != 0，置于 msg 之后、其余 attrs 之前
	if h.opts.AddSource && r.PC != 0 {
		if src, ok := sourceFromPC(r.PC); ok {
			sep()
			buf = append(buf, "source="...)
			buf = appendQuoted(buf, src)
		}
	}

	// 6) 其余 attrs：先 WithAttrs 累积的 h.attrs，再 record 自身 attrs
	//    （排除已输出的 trace_id/req_id），均带分组前缀、递归展开 Group。
	prefix := h.groupPrefix()
	buf = h.appendAttrs(buf, h.attrs, prefix)
	r.Attrs(func(a slog.Attr) bool {
		if isTraceIDAttr(a, traceID, reqID) {
			return true
		}
		buf = h.appendAttr(buf, a, prefix)
		return true
	})

	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()

	_, err := h.w.Write(buf)
	return err
}

// WithAttrs 返回追加属性的新实例（值拷贝，共享 w/opts/mu）
func (h *fileTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
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
func (h *fileTextHandler) WithGroup(name string) slog.Handler {
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
func (h *fileTextHandler) groupPrefix() string {
	if len(h.groups) == 0 {
		return ""
	}

	return strings.Join(h.groups, ".") + "."
}

// appendAttrs 批量输出属性
func (h *fileTextHandler) appendAttrs(buf []byte, attrs []slog.Attr, prefix string) []byte {
	for _, a := range attrs {
		buf = h.appendAttr(buf, a, prefix)
	}

	return buf
}

// appendAttr 输出单个属性，Group 类型递归展开（与 console 同构，去除颜色）
func (h *fileTextHandler) appendAttr(buf []byte, a slog.Attr, prefix string) []byte {
	if a.Value.Kind() == slog.KindGroup {
		inner := a.Value.Group()
		next := prefix + a.Key + "."
		for _, ga := range inner {
			buf = h.appendAttr(buf, ga, next)
		}

		return buf
	}

	buf = append(buf, ' ')
	buf = append(buf, prefix...)
	buf = append(buf, a.Key...)
	buf = append(buf, '=')

	return appendTextValue(buf, a.Value)
}

// needsQuoting 判断字符串是否需要加引号包裹：当且仅当包含空格、制表、'='、'"'、
// 反斜杠或控制字符（不可打印字符）。对齐标准 log/slog TextHandler 规则。
func needsQuoting(s string) bool {
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '=' || r == '"' || r == '\\' || !strconv.IsPrint(r) {
			return true
		}
	}

	return false
}

// appendQuoted 输出字符串：需要时以双引号包裹并转义（" \ 及控制字符，由
// strconv.AppendQuote 处理），否则原样输出。
func appendQuoted(buf []byte, s string) []byte {
	if needsQuoting(s) {
		return strconv.AppendQuote(buf, s)
	}

	return append(buf, s...)
}

// appendTextValue 输出 slog.Value：字符串按 quoting 规则处理，其余 Kind 复用
// 同包 appendValue（其内部已处理各 Kind）。Group 理论上已在 appendAttr 展开。
func appendTextValue(buf []byte, v slog.Value) []byte {
	if v.Kind() == slog.KindString {
		return appendQuoted(buf, v.String())
	}

	return appendValue(buf, v)
}
