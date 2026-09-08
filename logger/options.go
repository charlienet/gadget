package logger

import (
	"io"
	"log/slog"
)

type Option func(*Options)

// Options 汇总 New 的全部可选项。sink（控制台 / 文件）按「存在性」开关：
//   - Console != nil（由 WithConsole 置位）→ 启用控制台 sink；
//   - File != ""（由 WithFile 置位）→ 启用文件 sink；
//   - 两者皆无时 rebuild 兜底启用一个 stdout 控制台（见 default.go）。
//
// 故 Options 不再持有独立的 Out/Color/FileFormat 字段：控制台 writer/颜色归
// ConsoleSettings，文件格式归 FileOptions.Format。
type Options struct {
	Level         slog.Level        // 默认 slog.LevelInfo
	Console       *ConsoleSettings  // 控制台 sink（nil=未声明 WithConsole）
	File          string            // 文件路径，空 = 不启用文件 sink
	FileOpts      *FileOptions      // 文件轮换 + 输出格式配置（nil 用默认）
	Source        bool              // 是否输出源码位置 file:line
	Async         bool              // 异步写入：日志队列后台消费，不阻塞主业务
	QueueSize     int               // 异步队列容量（<=0 默认 10240）
	AsyncBlocking bool              // 异步队列满时阻塞背压（默认 false=丢弃并计数）
	Sensitive     *SensitiveOptions // 敏感信息过滤（nil 不启用）
	StackTrace    bool              // 错误堆栈：对 Wrap 过的错误自动附加 stack 属性
	Sampling      *SamplingOptions  // 日志采样（nil 不启用）
	Service       string            // 服务名：非空时 New 预置 service 属性
	Env           string            // 运行环境：非空时 New 预置 env 属性
	Leveler       slog.Leveler      // 自定义级别来源：非 nil 时优先于 Level/包级 SetLevel 动态调级
}

// ConsoleSettings 控制台 sink 的可选覆盖；nil（Options.Console）表示调用方未声明 WithConsole。
// 与 sink 存在性解耦：WithConsole() 即便不带子选项也会把 Console 置为非 nil，用于「启用控制台」。
type ConsoleSettings struct {
	Writer io.Writer // nil → 默认 os.Stdout
	Color  *bool     // nil → 自动（NO_COLOR 环境变量 + IsTerminal(Writer)）
}

func WithLevel(lvl slog.Level) Option {
	return func(o *Options) {
		o.Level = lvl
	}
}

// ---- 控制台 sink ----

// ConsoleOption 控制台 sink 的子选项（作用于 ConsoleSettings）。
type ConsoleOption func(*ConsoleSettings)

// WithConsole 启用控制台 sink。必须把 o.Console 置为非 nil（即使不带子选项），
// 再依次应用 opts。多次调用会叠加到同一个 ConsoleSettings（精调 writer/color 而非重置），
// 因此 WithConsole(WithConsoleWriter(w)) + WithConsole(WithConsoleColor(false)) 组合成立。
func WithConsole(opts ...ConsoleOption) Option {
	return func(o *Options) {
		if o.Console == nil {
			o.Console = &ConsoleSettings{}
		}
		for _, opt := range opts {
			opt(o.Console)
		}
	}
}

// WithConsoleWriter 指定控制台输出 writer（默认 os.Stdout）。
func WithConsoleWriter(w io.Writer) ConsoleOption {
	return func(c *ConsoleSettings) {
		c.Writer = w
	}
}

// WithConsoleColor 控制台颜色显式开关（覆盖自动判定）。
func WithConsoleColor(enabled bool) ConsoleOption {
	return func(c *ConsoleSettings) {
		c.Color = &enabled
	}
}

// WithService 设置服务名（非空时注入为 service 日志属性，随每条日志输出）
func WithService(name string) Option {
	return func(o *Options) {
		o.Service = name
	}
}

// WithEnv 设置运行环境标识（非空时注入为 env 日志属性，随每条日志输出）
func WithEnv(env string) Option {
	return func(o *Options) {
		o.Env = env
	}
}

// WithLeveler 指定自定义级别来源（如原子配置、按请求动态开关）。
// 非 nil 时优先于 WithLevel 的静态级别：handler 级别判断完全经该 Leveler，
// 包级 SetLevel 对该实例无效（级别控制权在调用方）。
func WithLeveler(l slog.Leveler) Option {
	return func(o *Options) {
		o.Leveler = l
	}
}

// WithFile 设置文件 sink（可轮换 + 输出格式），与 WithConsole 组合 = 双端输出（控制台 + 文件）。
// 文件格式经子选项 WithFormat(FormatText) 指定（默认 FormatJSON，仅作用于文件 handler）。
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

// WithSource 输出源码位置 file:line
func WithSource(enabled bool) Option {
	return func(o *Options) {
		o.Source = enabled
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

// FileFormat 文件输出格式枚举（string 基类型，与 Config.FileFormat 的 yaml 值同源）。
// 零值 ""（等价 FormatJSON）保证「未设置 = JSON」向后兼容；typed 参数防止调用方误传裸 string。
type FileFormat string

const (
	// FormatJSON 标准 slog.NewJSONHandler（默认）。
	FormatJSON FileFormat = "json"
	// FormatText 自研排序 text handler（time/level/trace_id/req_id/msg/source/其余 attrs）。
	FormatText FileFormat = "text"
)

// String 返回格式名（满足 fmt.Stringer；底层即格式字符串，直接返回）。
func (f FileFormat) String() string { return string(f) }

type FileOption func(*FileOptions)

type FileOptions struct {
	MaxSize    int        // MB，默认 100
	MaxAge     int        // 天，默认 30
	MaxBackups int        // 默认 10
	Compress   bool       // 默认 true
	Layout     string     // 非空 = 按日期轮换（rotate.RotateDateWriter）；空 = 按大小轮换（lumberjack）
	Format     FileFormat // 文件输出格式，零值 ""/FormatJSON 均输出 JSON（向后兼容）
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

// WithFormat 设置文件输出格式（FormatJSON 默认 / FormatText）。仅作用于文件 handler，
// 控制台输出不受影响。FormatText 为自研排序 handler（time/level/trace_id/req_id/msg/source/
// 其余 attrs），详见 text_file.go。
func WithFormat(format FileFormat) FileOption {
	return func(f *FileOptions) {
		f.Format = format
	}
}
