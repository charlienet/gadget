package logger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// ExitFunc 由包级 Fatal/Fatalf 在记录日志后调用。
// 测试中替换为无副作用函数，防止进程真实退出。
var ExitFunc = os.Exit

// DefaultLogger 包级默认 logger。New 内部已 slog.SetDefault 接入，
// slog 包级函数（slog.Info / slog.InfoContext 等）开箱即用；
// Init(cfg) 会重建并替换本变量（替换前的旧默认实例由 New/Init 关闭）。
//
// ⚠ 引用失效契约（N-1）：后续任何 New / Init 调用都会关闭旧默认实例
// （flush 异步队列后，异步链对其静默丢弃；文件 writer 借「写时重开」复活，
// 产生永不关闭的句柄）——先前从 DefaultLogger / New 返回值捕获的一切
// *slog.Logger 引用自此变为「陈旧且已关闭」。长生命周期组件（如 cache 的告警
// 日志、Fatal 出口）**不应跨重建持有引用**：应用重建 logger 后必须重新获取
// logger.DefaultLogger 或重新构造组件。
//
// 包内对 DefaultLogger / defaultLeveler / defaultInstance 的读写统一经
// defaultMu（见 M-5）；外部在初始化主线程直接赋值本变量（单线程惯用法）
// 与包内读写并发时不保证可见性，属既有约定。
var DefaultLogger *slog.Logger = New()

// defaultMu 保护 DefaultLogger / defaultLeveler / defaultInstance 三个包级状态：
// New/Init 写、SetLevel/Fatal 读，消除包内路径的 -race 可见竞争。
var defaultMu sync.Mutex

// defaultLeveler / defaultInstance 指向最近一次 New 创建的内部实例：
// SetLevel 经 defaultLeveler 即时调整默认 logger 级别；
// Fatal 经 defaultInstance flush 该实例的异步队列后再退出。
var (
	defaultLeveler  *DynamicLevel
	defaultInstance *slogLogger
)

// slogLogger：handler 链的内部装配结构（仅 New / rebuild / 包级 Close 使用，
// 对外暴露的日志 API 一律是 New 返回的原生 *slog.Logger）。
type slogLogger struct {
	mu            sync.RWMutex
	opt           Options        // 保存配置（含 sink 声明 opt.Console / opt.File，构造后固定）
	level         *DynamicLevel  // 动态级别（New 时创建一次；opt.Leveler 为 nil 时供 handler 使用，SetLevel 即时生效）
	slog          *slog.Logger   // New 组装完成后对外返回的 logger
	async         *AsyncHandler  // 当前生效的异步处理器（nil=未启用异步）
	fileWriter    io.Writer      // 文件输出 writer（New 时构建一次；nil=无文件输出）
	fileCloser    io.Closer      // 文件 writer 的 Close（包级 Close 时释放句柄；nil=无文件输出）
	syslogHandler *syslogHandler // syslog handler（rebuild 构建一次；nil=无 syslog sink）
	syslogCloser  io.Closer      // syslog 连接的 Close（包级 Close 时释放；nil=无 syslog sink）
	httpHandler   *httpHandler   // http handler（New 时构建一次并启动 worker；nil=无 http sink）
	httpCloser    io.Closer      // http sink 的 Close（停 worker + 收尾 flush；nil=无 http sink）
	closeOnce     sync.Once      // 保证 async/fileCloser/syslogCloser/httpCloser 只关闭一次（幂等）
}

// buildOptions 把 opts 依次应用到默认基线（Level=slog.LevelInfo，不含任何 sink）之上，
// 返回最终 Options。New 与 Init 共用，保证两处基线默认值与合并语义单一来源。
func buildOptions(opts ...Option) Options {
	o := Options{Level: slog.LevelInfo}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// New 组装 handler 链并返回原生 *slog.Logger：
//   - 默认 Level=slog.LevelInfo；sink 按存在性装配（WithConsole 启用控制台、WithFile 启用文件、
//     WithSyslog 启用 syslog、WithHTTP 启用 http），皆未声明时兜底一个 stdout 控制台，保证 DefaultLogger=New() 零配置可用；
//   - Service/Env 非空时以 With 预置 service/env 属性；
//   - 注册到包级注册表（包级 Close/Stats 统一释放），并 slog.SetDefault 接入；
//   - 替换默认实例引用前，先 close 旧默认实例（flush 异步队列、关文件句柄、
//     注销注册表），避免连续 New/Init 导致队列与句柄累积；首次（nil）跳过。
//
// 打日志直接使用 log/slog 原生 API（l.Info / l.InfoContext(ctx, msg, args...) 等），
// 所有 *Context 方法经链最外层 TraceHandler 自动提取 ctx 中的 trace_id/req_id 注入为日志属性。
// lumberjack 为惰性 IO，文件创建失败不会在 New 时返回 error。
func New(opts ...Option) *slog.Logger {
	opt := buildOptions(opts...)

	sl := newSlogLogger(opt)
	registerLogger(sl)

	defaultMu.Lock()
	if prev := defaultInstance; prev != nil {
		// N-3：关闭上一个默认实例（空队列立即返回，锁内等待代价可忽略）。
		// ⚠ flush 结果在此丢失是有意取舍：New 签名不返回 error，旧实例排空失败/
		// 超时（残余日志、句柄借写时重开）无法向调用方上抛信号。对落盘有强承诺
		// 的场景，替换前应显式调用包级 Close(timeout)（可加大 timeout、检查返回
		// 错误），或保持单一默认实例贯穿进程生命周期、仅在退出时 Close。
		_ = prev.close(2 * time.Second)
	}
	defaultLeveler = sl.level
	defaultInstance = sl
	defaultMu.Unlock()

	lg := sl.slog
	var preset []any
	if opt.Service != "" {
		preset = append(preset, slog.String(AttrService, opt.Service))
	}
	if opt.Env != "" {
		preset = append(preset, slog.String(AttrEnv, opt.Env))
	}
	if len(preset) > 0 {
		lg = lg.With(preset...)
	}

	slog.SetDefault(lg)
	return lg
}

// newSlogLogger 构建内部实例（不注册、不 SetDefault，由 New 统一编排）
func newSlogLogger(opt Options) *slogLogger {
	l := &slogLogger{
		opt:   opt,
		level: NewDynamicLevel(opt.Level),
	}

	// 文件 writer 只在 New 时构建一次：文件路径/轮换配置（MaxSize/MaxAge/MaxBackups/Compress/Layout）
	// 运行时不可变更。文件句柄由包级 Close 遍历注册表统一释放。
	if opt.File != "" {
		fw := buildFileWriter(opt.File, opt.FileOpts)
		l.fileWriter = fw
		l.fileCloser = fw
	}

	// syslog handler 只在 New 时构建一次（连接懒建/后台重关，句柄由包级 Close 释放）。
	// 级别来源与 rebuild 一致：显式 Leveler 优先，否则内部 DynamicLevel。
	if opt.Syslog != nil {
		var lvl slog.Leveler = l.level
		if opt.Leveler != nil {
			lvl = opt.Leveler
		}
		h, sc := newSyslogHandler(opt.Syslog, opt.Service, lvl, opt.Source)
		l.syslogHandler = h
		l.syslogCloser = sc
	}

	// http handler 同样只在 New 时构建一次（此处启动发送 worker，Close 必停，防 goroutine
	// 泄漏；每实例独立 worker + 连接池）。级别来源与 rebuild/syslog 一致。
	if opt.HTTP != nil {
		var lvl slog.Leveler = l.level
		if opt.Leveler != nil {
			lvl = opt.Leveler
		}
		h, s := newHTTPHandler(opt.HTTP, opt.Service, lvl, opt.Source)
		l.httpHandler = h
		l.httpCloser = s
	}

	l.rebuild()
	return l
}

// rebuild 按当前 opt 构建 handler 链与 l.slog（仅在 newSlogLogger 调用一次；
// 动态调级走 Leveler/DynamicLevel，不触发重建）。
// sink 按存在性装配（见 shouldEnableConsole）：WithConsole 启用控制台、WithFile 启用文件、
// WithSyslog 启用 syslog、WithHTTP 启用 http，两者以上并存时 MultiHandler 扇出；皆无则兜底 stdout 控制台。
// 链顺序（内→外）：console / file / syslog / http handler（并存时 MultiHandler）
// → StackHandler(可选) → SensitiveHandler(可选) → SamplingHandler(可选) → AsyncHandler(可选)
// → TraceHandler（内置总是包裹，位于最外层）。
// Trace 放最外层：trace_id/req_id 在调用方 goroutine 内同步提取进 record 后才进入
// 后续装饰器与异步队列，AsyncHandler 无需在队列中携带 ctx（避免长命队列持有
// 请求级 ctx 的反模式）。
func (l *slogLogger) rebuild() {
	// 级别来源：显式 WithLeveler 优先；nil 时用内部 DynamicLevel(opt.Level)（包级 SetLevel 即时生效）
	var lvl slog.Leveler = l.level
	if l.opt.Leveler != nil {
		lvl = l.opt.Leveler
	}

	var handlers []slog.Handler

	// sink 存在性装配。hasConsole = 显式声明 WithConsole（o.Console != nil）；
	// hasFile = New 时 File != "" 已构建 fileWriter；hasSyslog = New 时已构建 syslogHandler；
	// hasHTTP = New 时已构建 httpHandler。
	// 控制台启用条件 = hasConsole || !(hasFile||hasSyslog||hasHTTP)（见 shouldEnableConsole）：
	//   - 零 sink（既无 Console 又无任何非 console sink）→ 兜底 stdout 控制台，保住 DefaultLogger=New()
	//     开箱即用、避免包级日志静默与 Fatal 黑洞；
	//   - 已声明 File / Syslog / HTTP 等非 console sink 且未声明 Console → 不写 stdout。
	hasConsole := l.opt.Console != nil
	hasFile := l.fileWriter != nil
	hasSyslog := l.syslogHandler != nil
	hasHTTP := l.httpHandler != nil
	// 通用「是否已声明非 console sink」判断：file / syslog / http 任一存在即不兜底
	// stdout 控制台。新增后端只需并入该表达式，勿在 shouldEnableConsole 内硬编码分支。
	hasNonConsoleSink := hasFile || hasSyslog || hasHTTP
	if shouldEnableConsole(hasConsole, hasNonConsoleSink) {
		writer := io.Writer(os.Stdout)
		if l.opt.Console != nil && l.opt.Console.Writer != nil {
			writer = l.opt.Console.Writer
		}
		if l.opt.Console != nil && l.opt.Console.Format == FormatJSON {
			// json 版式：与 file/syslog/http 的 json 渲染同源（newFileHandler(FormatJSON)，
			// 含 time 定制 2006-01-02 15:04:05.000）；writer 沿用现逻辑，级别同源，
			// 颜色设置失效（JSON handler 无 ANSI 语义）。
			handlers = append(handlers, newFileHandler(writer, FormatJSON, &slog.HandlerOptions{
				Level:     lvl,
				AddSource: l.opt.Source,
				ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
					if a.Key == slog.TimeKey && len(groups) == 0 {
						a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05.000"))
					}
					return a
				},
			}))
		} else {
			// text 版式（默认，含未声明/零值）：现逻辑逐字不动（颜色自动判定、NoColor 两态）。
			noColor := false
			if l.opt.Console != nil && l.opt.Console.Color != nil {
				noColor = !*l.opt.Console.Color
			} else {
				// 自动模式：NO_COLOR 环境变量或非 TTY 输出（管道/文件）时禁用颜色
				noColor = !ShouldColor() || !IsTerminal(writer)
			}
			handlers = append(handlers, NewConsoleHandler(writer, &ConsoleOptions{
				Level:     lvl,
				AddSource: l.opt.Source,
				NoColor:   noColor,
			}))
		}
	}

	// 文件 handler（JSON/text + 轮换），复用 New 时构建的 fileWriter；
	// 格式取 FileOpts.Format（FileFormat 枚举，零值即 FormatJSON，向后兼容）。
	if l.fileWriter != nil {
		format := FormatJSON
		if l.opt.FileOpts != nil {
			format = l.opt.FileOpts.Format
		}
		handlerOpts := &slog.HandlerOptions{
			Level:     lvl,
			AddSource: l.opt.Source,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				// 时间格式化为 "2006-01-02 15:04:05.000"（JSON handler 用；text 形态由 console 渲染器内部固定）
				if a.Key == slog.TimeKey && len(groups) == 0 {
					a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05.000"))
				}
				return a
			},
		}
		handlers = append(handlers, newFileHandler(l.fileWriter, format, handlerOpts))
	}

	// syslog handler：New 时已构建（连接懒建/后台重连），此处仅加入扇出。
	if l.syslogHandler != nil {
		handlers = append(handlers, l.syslogHandler)
	}

	// http handler：New 时已构建（发送 worker 已启动），此处仅加入扇出。
	if l.httpHandler != nil {
		handlers = append(handlers, l.httpHandler)
	}

	var handler slog.Handler
	if len(handlers) == 1 {
		handler = handlers[0]
	} else {
		handler = slog.NewMultiHandler(handlers...)
	}

	// 错误堆栈：对 Wrap 过的 error 属性自动附加 stack 属性
	if l.opt.StackTrace {
		handler = NewStackHandler(handler)
	}
	// 敏感信息过滤：敏感 key 属性值打码（含 Group 递归）
	if l.opt.Sensitive != nil {
		handler = NewSensitiveHandler(handler, l.opt.Sensitive)
	}
	// 日志采样：窗口内按（级别+消息）计数采样
	if l.opt.Sampling != nil {
		handler = NewSamplingHandler(handler, l.opt.Sampling)
	}

	// 异步写入：handler 链外层包 AsyncHandler，入队后台消费。
	// ctx 不入队：trace 属性已由最外层 TraceHandler 在入队前写入 record。
	l.async = nil
	if l.opt.Async {
		queueSize := l.opt.QueueSize
		if queueSize <= 0 {
			queueSize = defaultAsyncQueueSize
		}
		l.async = NewAsyncHandler(handler, queueSize, l.opt.AsyncBlocking)
		handler = l.async
	}

	// trace_id/req_id 自动注入：内置默认行为，总是包裹且位于最外层——
	// Handle 最先执行，*Context 方法的 ctx 在调用方 goroutine 内即被提取进 record。
	handler = NewTraceHandler(handler)

	l.slog = slog.New(handler)
}

// shouldEnableConsole 依 sink 存在性决定控制台 handler 是否启用：显式声明 WithConsole
// （hasConsole），或"完全无 sink"（hasConsole 与 hasNonConsoleSink 皆 false）时兜底一个
// stdout 控制台，避免包级日志静默与 Fatal 黑洞；已声明任一非 console sink（file / syslog /
// http——由调用方并入 hasNonConsoleSink 表达式）且未声明 Console 则关闭控制台（纯该 sink 输出）。
// 参数语义泛化为「是否已声明任一非 console sink」，新增后端不在此函数内加分支。
func shouldEnableConsole(hasConsole, hasNonConsoleSink bool) bool {
	return hasConsole || !hasNonConsoleSink
}

// newFileHandler 按 format 枚举选择文件输出后端（仅作用于文件 handler，控制台不受影响）：
//   - FormatText → console 渲染器（console.go）的 NoColor 形态，字段顺序固定为
//     time/level/service(如有)/env(如有)/trace_id(如有)/req_id(如有)/msg/其余 attrs/source(可选，行尾)，
//     时间格式由渲染器内部固定为 "2006-01-02 15:04:05.000"（不吃标准 HandlerOptions 的 TimeFormat）；
//   - FormatJSON（默认，含任何非 FormatText 值）→ slog.NewJSONHandler，保持既有行为。
//
// handlerOpts 由调用方（rebuild）统一构建：JSON 分支完整复用（Level/AddSource/ReplaceAttr 时间定制）；
// text 分支复用 console 渲染器，不识别标准 HandlerOptions，仅从中取 Level/AddSource 两个字段。
func newFileHandler(w io.Writer, format FileFormat, handlerOpts *slog.HandlerOptions) slog.Handler {
	switch format {
	case FormatJSON:
		return slog.NewJSONHandler(w, handlerOpts)
	case FormatText:
		return NewConsoleHandler(w, &ConsoleOptions{
			Level:     handlerOpts.Level,
			AddSource: handlerOpts.AddSource,
			NoColor:   true,
		})
	default: // 零值 ""（语义等同 JSON）及未来新增未知值回退 JSON；
		// 显式枚举分支让 exhaustive 类 linter 在新增 FileFormat 常量时报警提醒补分支
		return slog.NewJSONHandler(w, handlerOpts)
	}
}

// close 内部实现（包级 Close / New 替换默认实例时调用）：关闭实例持有的异步处理器
// （flush 队列，确保日志落盘）、文件 writer、syslog 连接与 http sink（停 worker + 收尾 flush），
// 并从包级注册表注销。幂等：重复调用安全。
// timeout <= 0 时 async 内部用默认 2s。
//
// ⚠ 超时语义（M-4）：返回的错误表示异步队列未在 timeout 内排空——消费 goroutine
// 此后仍会持底层 writer 继续写入，且文件 writer 内部「写时重开」可能产生永不关闭的
// 新句柄。调用方收到非 nil 返回即视该实例存在残余，不应再复用该实例，只能假定
// 进程即将退出；需要绝对落盘的场景应传入足够大的 timeout 或改用同步 writer。
func (l *slogLogger) close(timeout time.Duration) error {
	var err error
	l.closeOnce.Do(func() {
		l.mu.RLock()
		a := l.async
		fc := l.fileCloser
		sc := l.syslogCloser
		hc := l.httpCloser
		l.mu.RUnlock()

		if a != nil {
			if e := a.Close(timeout); e != nil {
				err = errors.Join(err, e)
			}
		}
		if fc != nil {
			if e := fc.Close(); e != nil {
				err = errors.Join(err, e)
			}
		}
		if sc != nil {
			if e := sc.Close(); e != nil {
				err = errors.Join(err, e)
			}
		}
		// httpCloser 放最后：其 Close 内含「收尾批一次快速 POST + 等 inflight 落定」，
		// 最长可占 2×Timeout，置于文件/syslog 句柄释放之后，避免拖延其它资源回收。
		if hc != nil {
			if e := hc.Close(); e != nil {
				err = errors.Join(err, e)
			}
		}
	})
	unregisterLogger(l)
	return err
}

// SetLevel 包级动态调整默认 logger 的日志级别（对最近一次 New/Init 创建的实例即时生效）。
// 注意：若该实例经 WithLeveler 传入自定义 Leveler，内部 DynamicLevel 不参与级别判断，
// 本方法对其无效（级别控制权在用户的 Leveler）。
func SetLevel(lvl Level) {
	defaultMu.Lock()
	l := defaultLeveler
	defaultMu.Unlock()

	if l != nil {
		l.Set(lvl)
	}
}

// fatalCloseTimeout Fatal 退出前执行 sink 释放链的限时预算（与 New 替换默认实例时
// 调用的 prev.close(2*time.Second) 同量级，超时残余按「丢弃 + 限流告警」处理）。
const fatalCloseTimeout = 2 * time.Second

// Fatal 以 Fatal 级别经 DefaultLogger 记录（args 为 slog 风格成对属性），
// 再对当前默认实例执行**完整 sink 释放链**（async flush → 文件 → syslog → http 收尾投递），
// 最后调用 ExitFunc(1) 退出。
//
// 为什么走 close 而非只 flush 异步队列：http sink 的投递在 worker goroutine 里按
// BatchSize / FlushInterval 触发，Fatal 若只 flush 异步队列，缓冲批（最迟 FlushInterval
// 窗口内的全部日志，含这条 Fatal 本身）会随进程退出丢失。close 内含 async.Close(timeout)，
// 异步语义不丢；file/syslog 部分由 closeOnce 保证幂等，与并发的 New/Init 旧实例关闭互不重复。
//
// 加锁顺序：defaultMu 只用于取 DefaultLogger / defaultInstance 引用（取完立即释放），
// close 在其**锁外**执行——slogLogger.close 内部只取实例自身 l.mu 与注册表 loggerMu，
// 不取 defaultMu，锁外调用可确保不会与本函数（或 close 链内任何回调）形成自锁死锁。
// flush 超时错误尽力处理：进程即将退出，不再上抛。
func Fatal(msg string, args ...any) {
	defaultMu.Lock()
	lg := DefaultLogger
	inst := defaultInstance
	defaultMu.Unlock()

	lg.Log(context.Background(), FatalLevel, msg, args...)
	if inst != nil {
		// 致命错误必须尽力落盘后再退出：完整释放链确保远端 sink 的缓冲批也被送出。
		_ = inst.close(fatalCloseTimeout)
	}
	ExitFunc(1)
}

// Fatalf 以 Fatal 级别记录格式化消息后退出（仅格式化消息文本，不接收属性对）
func Fatalf(format string, args ...any) {
	Fatal(fmt.Sprintf(format, args...))
}
