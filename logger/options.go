package logger

import (
	"io"
	"log/slog"
	"os"
)

type Option func(*Options)

type Options struct {
	Level         slog.Level        // 默认 slog.LevelInfo
	Out           io.Writer         // 默认 os.Stderr（控制台输出）
	File          string            // 文件路径，空 = 不写文件
	FileOpts      *FileOptions      // 文件轮换配置（nil 用默认）
	Color         *bool             // 控制台颜色：nil=自动(NO_COLOR 环境变量)，非 nil 显式开关
	Source        bool              // 是否输出源码位置 file:line
	Recorder      LogRecorder       // 非 nil 时走旧适配器路径（logrus 插件兼容）
	Async         bool              // 异步写入：日志队列后台消费，不阻塞主业务
	QueueSize     int               // 异步队列容量（<=0 默认 10240）
	AsyncBlocking bool              // 异步队列满时阻塞背压（默认 false=丢弃并计数）
	Sensitive     *SensitiveOptions // 敏感信息过滤（nil 不启用）
	StackTrace    bool              // 错误堆栈：对 Wrap 过的错误自动附加 stack 属性
	Sampling      *SamplingOptions  // 日志采样（nil 不启用）
}

func New(opts ...Option) Logger {
	opt := Options{
		Level: slog.LevelInfo,
		Out:   os.Stderr,
	}

	for _, o := range opts {
		o(&opt)
	}

	if opt.Recorder != nil {
		// 旧适配器路径（外部插件，如 logrus）：仅负责基础输出/级别/文件落盘。
		// 能力边界：Sensitive/StackTrace/Sampling/Async 只在默认 slog 实现中生效，
		// recorder 路径无法承载（静默忽略 = 安全机制静默失效），显式失败优于静默失效。
		if opt.Sensitive != nil || opt.StackTrace || opt.Sampling != nil || opt.Async {
			panic("logger: recorder 路径不支持 Sensitive/StackTrace/Sampling/Async 选项，请使用默认 slog 实现或移除这些选项")
		}

		// 文件输出与轮换由 logger 包统一负责，插件只管格式，落盘开箱即用。
		var fw io.Writer
		if opt.File != "" {
			fwc := buildFileWriter(opt.File, opt.FileOpts) // nil FileOpts 内部用默认配置
			fw = fwc
			opt.Out = io.MultiWriter(opt.Out, fw) // 插件原样输出同时落盘
			registerFileCloser(fwc)               // 包级 Close 时释放文件句柄
		}
		opt.Recorder.Init(opt)
		return newHelper(opt, opt.Recorder, fw)
	}

	// 默认 slog 实现
	return newSlogLogger(opt)
}

func WithLevel(lvl slog.Level) Option {
	return func(o *Options) {
		o.Level = lvl
	}
}

func WithOutput(output io.Writer) Option {
	return func(o *Options) {
		o.Out = output
	}
}

// WithFile 设置文件输出（可轮换），与 WithOutput 组合 = 双路输出
func WithFile(path string, fopts ...FileOption) Option {
	return func(o *Options) {
		fo := defaultFileOptions()
		for _, f := range fopts {
			f(fo)
		}

		o.File = path
		o.FileOpts = fo
	}
}

// WithColor 控制台颜色显式开关
func WithColor(enabled bool) Option {
	return func(o *Options) {
		o.Color = &enabled
	}
}

// WithSource 输出源码位置 file:line
func WithSource(enabled bool) Option {
	return func(o *Options) {
		o.Source = enabled
	}
}

// WithRecorder 兼容外部插件（logrus）
func WithRecorder(r LogRecorder) Option {
	return func(o *Options) {
		o.Recorder = r
	}
}

// WithAsync 启用异步写入：日志队列后台消费，不阻塞主业务。
// queueSize 为队列容量（可省略，默认 10240）；队列满时默认丢弃并计数（可用 Stats 观测），
// 搭配 WithAsyncBlocking 可改为阻塞背压。
func WithAsync(queueSize ...int) Option {
	return func(o *Options) {
		o.Async = true
		if len(queueSize) > 0 {
			o.QueueSize = queueSize[0]
		}
	}
}

// WithAsyncBlocking 队列满时阻塞等待（背压，绝不丢日志；极端洪峰下会反压调用方）。
func WithAsyncBlocking() Option {
	return func(o *Options) {
		o.AsyncBlocking = true
	}
}

// ---- 敏感信息过滤 ----

// WithSensitiveKeys 追加敏感字段 key（子串匹配、大小写不敏感）。
// 与内置默认词集（精确匹配）合并生效：内置词只精确命中，用户显式指定的词按子串匹配。
func WithSensitiveKeys(keys ...string) Option {
	return func(o *Options) {
		if o.Sensitive == nil {
			o.Sensitive = &SensitiveOptions{}
		}
		o.Sensitive.Keys = append(o.Sensitive.Keys, keys...)
	}
}

// WithSensitiveMask 自定义掩码（默认 "******"）
func WithSensitiveMask(mask string) Option {
	return func(o *Options) {
		if o.Sensitive == nil {
			o.Sensitive = &SensitiveOptions{}
		}
		o.Sensitive.Mask = mask
	}
}

// WithSensitiveMatch 自定义敏感匹配函数（优先于默认子串匹配）
func WithSensitiveMatch(match func(key string) bool) Option {
	return func(o *Options) {
		if o.Sensitive == nil {
			o.Sensitive = &SensitiveOptions{}
		}
		o.Sensitive.Match = match
	}
}

// ---- 错误堆栈 ----

// WithStackTrace 启用错误堆栈（对 Wrap 过的错误自动附加 stack 属性）
func WithStackTrace(enabled bool) Option {
	return func(o *Options) {
		o.StackTrace = enabled
	}
}

// ---- 日志采样 ----

// WithSampling 启用日志采样（first 条保留，之后每 thereafter 条保留 1 条）
func WithSampling(first, thereafter int) Option {
	return func(o *Options) {
		o.Sampling = &SamplingOptions{First: first, Thereafter: thereafter}
	}
}

// ---- 文件轮换选项 ----

type FileOption func(*FileOptions)

type FileOptions struct {
	MaxSize    int    // MB，默认 100
	MaxAge     int    // 天，默认 30
	MaxBackups int    // 默认 10
	Compress   bool   // 默认 true
	Layout     string // 非空 = 按日期轮换（rotate.RotateDateWriter）；空 = 按大小轮换（lumberjack）
}

func WithMaxSize(mb int) FileOption {
	return func(f *FileOptions) {
		f.MaxSize = mb
	}
}

func WithMaxAge(days int) FileOption {
	return func(f *FileOptions) {
		f.MaxAge = days
	}
}

func WithMaxBackups(n int) FileOption {
	return func(f *FileOptions) {
		f.MaxBackups = n
	}
}

func WithCompress(b bool) FileOption {
	return func(f *FileOptions) {
		f.Compress = b
	}
}

// WithDateRotate 启用按日期轮换（layout 如 "2006-01-02"）
func WithDateRotate(layout string) FileOption {
	return func(f *FileOptions) {
		f.Layout = layout
	}
}
