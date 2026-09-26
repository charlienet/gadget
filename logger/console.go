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

// consoleHandler 彩色控制台 slog.Handler，同时是文件 text sink（FormatText）的
// 渲染实现——文件通道以 NoColor: true 形态复用本 handler（见 newFileHandler），
// 两通道字节等同由单一实现构造性成立。
// 字段次序：time → level → service/env/trace_id/req_id（如有）→ msg → 其余 attrs →
// source（若启用，恒在行尾）。
// 注意：mu 用指针共享，WithAttrs/WithGroup 派生时值拷贝安全；
// w 构造后固定（输出目标切换 = 重建 logger，不支持热替换）。
type consoleHandler struct {
	mu     *sync.Mutex
	w      io.Writer
	opts   ConsoleOptions
	attrs  []slog.Attr
	groups []string
}

// NewConsoleHandler 构造彩色控制台 handler：w 为输出目标（如 os.Stdout），
// opts 为值拷贝，派生实例共享同一 writer 与互斥锁。
func NewConsoleHandler(w io.Writer, opts *ConsoleOptions) slog.Handler {
	return &consoleHandler{mu: &sync.Mutex{}, w: w, opts: *opts}
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

// Handle 拼装完整行后一次性写入（整行加锁，避免并发写交错）。
// 字段次序：time → level → service/env/trace_id/req_id（如有）→ msg → 其余 attrs →
// source（若启用，恒在行尾）。字段间以 sep() 补单个空格：首个字段（time 零值时被跳过）
// 前不留行首空格。
func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
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

	// 时间戳（彩色），格式 "2006-01-02 15:04:05.000"
	if !r.Time.IsZero() {
		sep()
		if !h.opts.NoColor {
			buf = append(buf, colorTimestamp...)
		}
		buf = append(buf, r.Time.Format("2006-01-02 15:04:05.000")...)
		if !h.opts.NoColor {
			buf = append(buf, colorReset...)
		}
	}

	// 级别（彩色）：裸词无括号；ANSI 仅叠加在级别词本身；time 零值时不留行首空格
	sep()
	if !h.opts.NoColor {
		buf = append(buf, levelColor(r.Level)...)
	}
	buf = append(buf, formatLevel(r.Level)...)
	if !h.opts.NoColor {
		buf = append(buf, colorReset...)
	}

	// 前置字段 service/env/trace_id/req_id：双源挑选判据见 record_fields.go，
	// 按 frontFieldKeys 固定次序前置到消息之前。
	// 版式——裸值、无 key= 前缀、按 needsQuoting 规则可选加引号；
	// NoColor 时整行与文件 text sink 等同（同一实现），彩色仅在时间/级别词/attr key 上叠加。
	picked := pickFrontFields(r, h.attrs, h.groups)
	for _, key := range frontFieldKeys {
		if v := picked.get(key); v != "" {
			sep()
			buf = appendQuoted(buf, v)
		}
	}

	// 消息本体：\n/\r 转义为字面量保持单行，不上色、不加引号（见 appendMessageEscaped）
	sep()
	buf = appendMessageEscaped(buf, r.Message)

	// 属性：先输出 WithAttrs 累积的 h.attrs，再输出记录自身 attrs
	// （两循环均跳过已前置的前置字段，避免消息之后重复；h.attrs 带分组前缀时
	// 其 key 与前置的裸 key 不同名，不参与去重），均带分组前缀
	prefix := h.groupPrefix()
	buf = h.appendAttrs(buf, h.attrs, prefix, picked, 0)
	r.Attrs(func(a slog.Attr) bool {
		// prefixed 传 false：record attr 的 key 恒为裸 key、不随 handler groups 变化，
		// 故已前置的前置字段必与之同名、须剔除（勿改为传 prefix != ""）。
		if isPickedFrontAttr(a, picked, false) {
			return true
		}
		buf = h.appendAttr(buf, a, prefix, 0)
		return true
	})

	// 源码位置：仅当 AddSource 且 r.PC != 0，恒在行尾（其余 attrs 之后）。
	// source= 保持 k=v，值走 appendQuoted；彩色模式下 key 不上色（与 attrs 染色 key 有意不同）。
	if h.opts.AddSource && r.PC != 0 {
		if src, ok := sourceFromPC(r.PC); ok {
			sep()
			buf = append(buf, "source="...)
			buf = appendQuoted(buf, src)
		}
	}

	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()

	_, err := h.w.Write(buf)
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

// maxAttrRenderDepth Group 递归展开深度上限（对齐标准库 maxLogValues=100 量级）。
// 渲染层必须自设上限：maxLogValues 只在单次 Value.Resolve 调用链内累计，
// selfGroup 形态（LogValue 返回含自身惰性值的 Group）每次 Resolve 独立计数、
// 标准库拦不住渲染层递归；无上限时 prefix 逐层拼接为 O(depth²) 内存增长，
// 可先触发内核 OOM kill 波及同机进程（子进程实测 signal: killed）。
// 超限分支不再下钻，输出 <prefix+key>=!DEPTH 有界退化单行。
const maxAttrRenderDepth = 100

// appendAttrs 批量输出属性；跳过已前置输出的前置字段（prefixed 见 isPickedFrontAttr）。
// depth 为本批 attrs 的渲染起始深度（入口传 0，Group 递归逐级 +1）。
func (h *consoleHandler) appendAttrs(buf []byte, attrs []slog.Attr, prefix string, picked frontFields, depth int) []byte {
	prefixed := prefix != ""
	for _, a := range attrs {
		if isPickedFrontAttr(a, picked, prefixed) {
			continue
		}
		buf = h.appendAttr(buf, a, prefix, depth)
	}

	return buf
}

// appendAttr 输出单个属性，Group 类型递归展开（深度受 maxAttrRenderDepth 约束，
// 达上限不再下钻、输出字面 !DEPTH 退化，病理/程序化超深输入有界不 OOM 不栈爆）。
// 入口先 Resolve：实现 slog.LogValuer 的值 Kind() 为 KindLogValuer（Go 1.21+），
// 判 Kind 前不解析会漏进 appendValue 的 default 调 Group() 而 panic（对齐标准库
// handleState.appendAttr，log/slog/handler.go）。Group 元素递归回本函数逐级解析，
// 嵌套 LogValuer 由标准库承担（Value.Resolve 内建 maxLogValues，链式自引用超限
// 退化为 error）；跨 Resolve 调用的渲染层递归深度由本函数 depth 参数守卫。
func (h *consoleHandler) appendAttr(buf []byte, a slog.Attr, prefix string, depth int) []byte {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup && depth < maxAttrRenderDepth {
		inner := a.Value.Group()
		next := prefix + a.Key + "."
		for _, ga := range inner {
			buf = h.appendAttr(buf, ga, next, depth+1)
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

	// 深度上限退化：Group 不再下钻，值输出字面 !DEPTH（无特殊字符，
	// 按 needsQuoting 规则形态不加引号，此处直接拼接）
	if a.Value.Kind() == slog.KindGroup {
		return append(buf, "!DEPTH"...)
	}

	// 值渲染：string 按 needsQuoting 规则可选加引号，其余 Kind 复用 appendValue
	return appendTextValue(buf, a.Value)
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
		// KindGroup 理论上已在 appendAttr 展开，此处防御性兜底。
		// 惰性 LogValuer（KindLogValuer）会落入本分支：先 Resolve（保证返回值
		// 不再为 KindLogValuer），仍非 Group 则转派对应 Kind，确保任何输入不 panic。
		v = v.Resolve()
		if v.Kind() != slog.KindGroup {
			return appendValue(buf, v)
		}

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

// appendMessageEscaped 输出 msg 段：'\n' → 字面两字符 `\` `n`，'\r' → 字面 `\` `r`；
// 其余字节原样（含 \t、引号、空格），不加前后引号。目的：单条日志恒占一行，
// 防按 req_id/trace_id grep 过滤时消息换行断行。JSON 通道不经此处（标准库已转义）。
// 快路径：不含 \n\r 时直接 append 原文，零额外分配。
func appendMessageEscaped(buf []byte, msg string) []byte {
	if strings.IndexByte(msg, '\n') < 0 && strings.IndexByte(msg, '\r') < 0 {
		return append(buf, msg...)
	}

	for i := 0; i < len(msg); i++ {
		switch msg[i] {
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		default:
			buf = append(buf, msg[i])
		}
	}

	return buf
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
// 入口先 Resolve：惰性 LogValuer 解析后才可能是 KindString 形态，需纳入引号规则；
// 对已解析值为幂等空操作。
func appendTextValue(buf []byte, v slog.Value) []byte {
	v = v.Resolve()
	if v.Kind() == slog.KindString {
		return appendQuoted(buf, v.String())
	}

	return appendValue(buf, v)
}
