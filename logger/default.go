package logger

import (
	"context"
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
	mu         sync.RWMutex
	opt        Options       // 保存配置（含控制台输出目标 opt.Out，构造后固定）
	level      *DynamicLevel // 动态级别（New 时创建一次；opt.Leveler 为 nil 时供 handler 使用，SetLevel 即时生效）
	slog       *slog.Logger  // New 组装完成后对外返回的 logger
	async      *AsyncHandler // 当前生效的异步处理器（nil=未启用异步）
	fileWriter io.Writer     // 文件输出 writer（New 时构建一次；nil=无文件输出）
	fileCloser io.Closer     // 文件 writer 的 Close（包级 Close 时释放句柄；nil=无文件输出）
	closeOnce  sync.Once     // 保证 async/fileCloser 只关闭一次（幂等）
}

// New 组装 handler 链并返回原生 *slog.Logger：
//   - 默认 Level=slog.LevelInfo、Out=os.Stdout，Option 依次覆盖；
//   - Service/Env 非空时以 With 预置 service/env 属性；
//   - 注册到包级注册表（包级 Close/Stats 统一释放），并 slog.SetDefault 接入；
//   - 替换默认实例引用前，先 close 旧默认实例（flush 异步队列、关文件句柄、
//     注销注册表），避免连续 New/Init 导致队列与句柄累积；首次（nil）跳过。
//
// 打日志直接使用 log/slog 原生 API（l.Info / l.InfoContext(ctx, msg, args...) 等），
// 所有 *Context 方法经链最外层 TraceHandler 自动提取 ctx 中的 trace_id/req_id 注入为日志属性。
// lumberjack 为惰性 IO，文件创建失败不会在 New 时返回 error。
func New(opts ...Option) *slog.Logger {
	opt := Options{
		Level: slog.LevelInfo,
		Out:   os.Stdout,
	}

	for _, o := range opts {
		o(&opt)
	}

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
		preset = append(preset, slog.String("service", opt.Service))
	}
	if opt.Env != "" {
		preset = append(preset, slog.String("env", opt.Env))
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

	l.rebuild()
	return l
}

// rebuild 按当前 opt 构建 handler 链与 l.slog（仅在 newSlogLogger 调用一次；
// 动态调级走 Leveler/DynamicLevel，不触发重建）。
// 链顺序（内→外）：console(+file JSON 的 MultiHandler) → StackHandler(可选)
// → SensitiveHandler(可选) → SamplingHandler(可选) → AsyncHandler(可选)
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

	// 控制台 handler（默认 stdout）：writer 取 opt.Out，构造后固定
	noColor := false
	if l.opt.Color != nil {
		noColor = !*l.opt.Color
	} else {
		// 自动模式：NO_COLOR 环境变量或非 TTY 输出（管道/文件）时禁用颜色
		noColor = !ShouldColor() || !IsTerminal(l.opt.Out)
	}
	console := NewConsoleHandler(l.opt.Out, &ConsoleOptions{
		Level:     lvl,
		AddSource: l.opt.Source,
		NoColor:   noColor,
	})
	handlers = append(handlers, console)

	// 文件 handler（JSON + 轮换），复用 New 时构建的 fileWriter
	if l.fileWriter != nil {
		file := slog.NewJSONHandler(l.fileWriter, &slog.HandlerOptions{
			Level:     lvl,
			AddSource: l.opt.Source,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				// 时间格式化为 "2006-01-02 15:04:05.000"
				if a.Key == slog.TimeKey && len(groups) == 0 {
					a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05.000"))
				}
				return a
			},
		})
		handlers = append(handlers, file)
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

// close 内部实现（包级 Close / New 替换默认实例时调用）：关闭实例持有的异步处理器
// （flush 队列，确保日志落盘）与文件 writer，并从包级注册表注销。幂等：重复调用安全。
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
		l.mu.RUnlock()

		if a != nil {
			if e := a.Close(timeout); e != nil {
				err = e
			}
		}
		if fc != nil {
			if e := fc.Close(); e != nil {
				err = e
			}
		}
	})
	unregisterLogger(l)
	return err
}

// flushAsync 若启用异步，Close 当前 async 处理器并 flush 队列（幂等，无异步时直接跳过），
// 返回 AsyncHandler.Close 的结果（超时残余错误，调用方按需处理）。
// 包级 Fatal 退出前调用，尽力保证致命日志落盘。
func (l *slogLogger) flushAsync() error {
	l.mu.RLock()
	a := l.async
	l.mu.RUnlock()

	if a != nil {
		return a.Close(2 * time.Second)
	}
	return nil
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

// Fatal 以 Fatal 级别经 DefaultLogger 记录（args 为 slog 风格成对属性），
// flush 默认实例的异步队列后调用 ExitFunc(1) 退出。
// flush 超时错误尽力处理：进程即将退出，不再上抛。
func Fatal(msg string, args ...any) {
	defaultMu.Lock()
	lg := DefaultLogger
	inst := defaultInstance
	defaultMu.Unlock()

	lg.Log(context.Background(), FatalLevel, msg, args...)
	if inst != nil {
		_ = inst.flushAsync() // 致命错误必须尽力落盘后再退出
	}
	ExitFunc(1)
}

// Fatalf 以 Fatal 级别记录格式化消息后退出（仅格式化消息文本，不接收属性对）
func Fatalf(format string, args ...any) {
	Fatal(fmt.Sprintf(format, args...))
}
