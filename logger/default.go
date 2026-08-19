package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

var DefaultLogger = New()

// slogLogger：slog 默认实现
type slogLogger struct {
	mu         sync.RWMutex
	opt        Options       // 保存配置（SetOutput 时更新 Out）
	level      *DynamicLevel // 动态级别（New 时创建一次，SetLevel 即时生效，无需重建）
	slog       *slog.Logger
	async      *AsyncHandler              // 当前生效的异步处理器（nil=未启用异步）
	outPtr     *atomic.Pointer[io.Writer] // 控制台 writer 共享指针：SetOutput 热切换（原子换指针，不重建链）
	fileWriter io.Writer                  // 文件输出 writer（New 时构建一次；nil=无文件输出）
	fileCloser io.Closer                  // 文件 writer 的 Close（实例 Close 时释放句柄；nil=无文件输出）
	closeOnce  sync.Once                  // 保证 async/fileCloser 只关闭一次（幂等 Close）
}

func newSlogLogger(opt Options) Logger {
	outPtr := &atomic.Pointer[io.Writer]{}
	outPtr.Store(&opt.Out)

	l := &slogLogger{
		opt:    opt,
		level:  NewDynamicLevel(opt.Level),
		outPtr: outPtr,
	}

	// 文件 writer 只在 New 时构建一次：文件路径/轮换配置（MaxSize/MaxAge/MaxBackups/Compress/Layout）
	// 运行时不可变更，SetOutput 只影响控制台输出。
	// 文件句柄由实例 Close() 释放（不再注册到包级 fileClosersList，避免双重关闭）。
	if opt.File != "" {
		fw := buildFileWriter(opt.File, opt.FileOpts)
		l.fileWriter = fw
		l.fileCloser = fw
	}

	l.rebuild()
	registerLogger(l) // 注册到包级注册表（包级 Close 或实例 Close() 时释放）
	return l
}

// rebuild 按当前 opt 重建 handler 链与 l.slog（仅在 newSlogLogger 调用一次；
// SetLevel 走 DynamicLevel、SetOutput 走 writer 指针热切换，均不触发重建）
func (l *slogLogger) rebuild() {
	var handlers []slog.Handler

	// 控制台 handler（默认 stderr），共享 outPtr 实现 SetOutput 热切换
	noColor := false
	if l.opt.Color != nil {
		noColor = !*l.opt.Color
	} else if os.Getenv("NO_COLOR") != "" {
		noColor = true
	}
	console := newConsoleHandlerWithPtr(l.outPtr, &ConsoleOptions{
		Level:     l.level,
		AddSource: l.opt.Source,
		NoColor:   noColor,
	})
	handlers = append(handlers, console)

	// 文件 handler（JSON + 轮换），复用 New 时构建的 fileWriter
	if l.fileWriter != nil {
		file := slog.NewJSONHandler(l.fileWriter, &slog.HandlerOptions{
			Level:     l.level,
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
	// 不再有 rebuild 重建 async 的场景，无需先 Close 旧实例。
	// async 由实例 Close() 关闭（包级 Close 遍历实例时统一处理）。
	if l.opt.Async {
		queueSize := l.opt.QueueSize
		if queueSize <= 0 {
			queueSize = 10240
		}
		l.async = NewAsyncHandler(handler, queueSize, l.opt.AsyncBlocking)
		handler = l.async
	}

	l.slog = slog.New(handler)
}

// Close 关闭实例持有的异步处理器（flush 队列，确保日志落盘）与文件 writer，
// 并从包级注册表注销。幂等：重复调用安全。
// 注意：异步 logger 建议进程退出前调用包级 logger.Close()，或对关键实例调用本方法。
// 派生实例（With/WithGroup）共享 async 与 outPtr：任一实例 Close 后该共享队列停止接收。
func (l *slogLogger) Close() error {
	return l.close(0) // 0 = async 默认 2s 超时
}

// close 内部实现：timeout 由包级 Close 透传（<=0 时 async 内部用默认 2s）
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

func (l *slogLogger) WithField(key string, val any) Logger {
	l.mu.RLock()
	s := l.slog
	a := l.async
	opt := l.opt
	l.mu.RUnlock()

	// 派生实例共享 outPtr：SetOutput 后所有派生实例一起切换输出
	return &slogLogger{opt: opt, level: l.level, slog: s.With(key, val), async: a, outPtr: l.outPtr}
}

func (l *slogLogger) WithFields(fields map[string]any) Logger {
	l.mu.RLock()
	s := l.slog
	a := l.async
	opt := l.opt
	l.mu.RUnlock()

	// key 排序后按序 append：map 迭代顺序随机会导致派生日志的属性输出顺序不稳定
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	args := make([]any, 0, len(fields)*2)
	for _, k := range keys {
		args = append(args, k, fields[k])
	}

	return &slogLogger{opt: opt, level: l.level, slog: s.With(args...), async: a, outPtr: l.outPtr}
}

// With 追加 slog 风格字段（成对 key/value，奇数个时按 slog 规则补 !BADKEY）
func (l *slogLogger) With(args ...any) Logger {
	if len(args)%2 != 0 {
		args = append(args, "!BADKEY")
	}

	l.mu.RLock()
	s := l.slog
	a := l.async
	opt := l.opt
	l.mu.RUnlock()

	return &slogLogger{opt: opt, level: l.level, slog: s.With(args...), async: a, outPtr: l.outPtr}
}

// WithAttrs 追加 slog.Attr 风格字段。
// 用 slog.Any(a.Key, a.Value) 包装：slog.AnyValue 识别已构造的 slog.Value 直接透传，保持原始 Kind。
func (l *slogLogger) WithAttrs(attrs ...slog.Attr) Logger {
	args := make([]any, 0, len(attrs)*2)
	for _, a := range attrs {
		args = append(args, slog.Any(a.Key, a.Value))
	}

	l.mu.RLock()
	s := l.slog
	a := l.async
	opt := l.opt
	l.mu.RUnlock()

	return &slogLogger{opt: opt, level: l.level, slog: s.With(args...), async: a, outPtr: l.outPtr}
}

// LogAttrs 记录一条 slog.Attr 风格日志（slog 原生 LogAttrs 语义）
func (l *slogLogger) LogAttrs(level slog.Level, msg string, attrs ...slog.Attr) {
	l.mu.RLock()
	s := l.slog
	l.mu.RUnlock()

	s.LogAttrs(context.Background(), level, msg, attrs...)
}

// WithGroup 按 slog 分组语义派生新 logger（共享 opt/level/async/outPtr）
func (l *slogLogger) WithGroup(name string) Logger {
	l.mu.RLock()
	s := l.slog
	a := l.async
	opt := l.opt
	l.mu.RUnlock()

	return &slogLogger{opt: opt, level: l.level, slog: s.WithGroup(name), async: a, outPtr: l.outPtr}
}

func (l *slogLogger) SetLevel(lvl Level) {
	// 动态级别即时生效，无需重建 handler
	l.level.Set(lvl)
}

// SetOutput 热切换控制台 writer：原子替换共享指针内容，不重建 handler 链。
// 无锁持锁等待（原 rebuild 会 Close async 最长阻塞 2s，期间所有日志 RLock 排队）；
// 所有派生实例共享 outPtr，SetOutput 全局生效。
// 注意：文件输出路径/轮换配置在 New 时固定，SetOutput 不影响文件输出。
func (l *slogLogger) SetOutput(out io.Writer) {
	l.mu.Lock()
	l.opt.Out = out
	l.outPtr.Store(&out)
	l.mu.Unlock()
}

// Log 记录一条 slog 风格日志
func (l *slogLogger) Log(level slog.Level, msg string, args ...any) {
	l.mu.RLock()
	s := l.slog
	l.mu.RUnlock()

	s.Log(context.Background(), level, msg, args...)
}

// log 读快照后输出（slog.Logger 自身并发安全）。
// 快捷方法语义（Info/Debug/Error 等）：第一个参数为消息，其余参数按 slog 属性处理，
// 与 slog.Logger.Info(msg, args...) 一致，使敏感过滤/错误堆栈等装饰器对常用入口生效。
func (l *slogLogger) log(level slog.Level, args ...any) {
	l.mu.RLock()
	s := l.slog
	l.mu.RUnlock()

	if len(args) == 0 {
		return
	}
	s.Log(context.Background(), level, fmt.Sprint(args[0]), args[1:]...)
}

func (l *slogLogger) Info(args ...any) { l.log(Info, args...) }
func (l *slogLogger) Infof(template string, args ...any) {
	l.log(Info, fmt.Sprintf(template, args...))
}

func (l *slogLogger) Trace(args ...any) { l.log(Trace, args...) }
func (l *slogLogger) Tracef(template string, args ...any) {
	l.log(Trace, fmt.Sprintf(template, args...))
}

func (l *slogLogger) Debug(args ...any) { l.log(Debug, args...) }
func (l *slogLogger) Debugf(template string, args ...any) {
	l.log(Debug, fmt.Sprintf(template, args...))
}

func (l *slogLogger) Warn(args ...any) { l.log(Warn, args...) }
func (l *slogLogger) Warnf(template string, args ...any) {
	l.log(Warn, fmt.Sprintf(template, args...))
}

func (l *slogLogger) Error(args ...any) { l.log(Error, args...) }
func (l *slogLogger) Errorf(template string, args ...any) {
	l.log(Error, fmt.Sprintf(template, args...))
}

func (l *slogLogger) Fatal(args ...any) {
	l.log(Fatal, args...)
	l.flushAsync() // 致命错误必须落盘后再退出
	ExitFunc(1)
}

func (l *slogLogger) Fatalf(template string, args ...any) {
	l.log(Fatal, fmt.Sprintf(template, args...))
	l.flushAsync() // 致命错误必须落盘后再退出
	ExitFunc(1)
}

// flushAsync 若启用异步，Close 当前 async 处理器并 flush 队列（幂等，无异步时直接跳过）
func (l *slogLogger) flushAsync() {
	l.mu.RLock()
	a := l.async
	l.mu.RUnlock()

	if a != nil {
		a.Close(2 * time.Second)
	}
}

func (l *slogLogger) WithContext(parent context.Context) context.Context {
	return WithLogger(parent, l)
}
