// Package logrus 提供基于 logrus 的 LogRecorder 实现（recorder 路径兼容插件）。
//
// 注意：recorder 路径的 SetLevel/SetOutput 作用于底层 logrus.Logger 全局实例
// （l.SetLevel(...) / l.SetOutput(...)）。多个 logger 实例共享同一 logrus.Logger
// 时会互相影响（后调用的覆盖先调用的），建议单实例使用。
package logrus

import (
	"github.com/charlienet/gadget/logger"
	"github.com/sirupsen/logrus"
)

var DefaultLogger = logger.New(logger.WithRecorder(New()))

type entryLogger interface {
	WithFields(logrus.Fields) *logrus.Entry

	Log(level logrus.Level, args ...any)
	Logf(level logrus.Level, format string, args ...any)
}

type logrusLogger struct {
	Logger entryLogger
}

func New(opts ...Option) logger.LogRecorder {
	opt := Options{Formatter: &logrus.TextFormatter{}}
	for _, o := range opts {
		o(&opt)
	}

	l := logrus.New()
	l.SetFormatter(opt.Formatter)
	l.SetReportCaller(opt.ReportCaller)
	for _, hook := range opt.Hooks {
		l.AddHook(hook)
	}

	return &logrusLogger{Logger: l}
}

func (l *logrusLogger) Init(opt logger.Options) {
	switch ll := l.Logger.(type) {
	case *logrus.Logger:
		setOptions(ll, opt)
	case *logrus.Entry:
		setOptions(ll.Logger, opt)
	}
}

func setOptions(l *logrus.Logger, opt logger.Options) {
	l.SetLevel(loggerToLogrusLevel(opt.Level))
	l.SetOutput(opt.Out)
}

func (l *logrusLogger) Fields(fields map[string]any) logger.LogRecorder {
	return &logrusLogger{l.Logger.WithFields(fields)}
}

func (l *logrusLogger) Log(lvl logger.Level, args ...any) {
	l.Logger.Log(loggerToLogrusLevel(lvl), args...)
}

func (l *logrusLogger) Logf(level logger.Level, format string, args ...any) {

	l.Logger.Logf(loggerToLogrusLevel(level), format, args...)
}

func (*logrusLogger) String() string { return "logrus" }

// loggerToLogrusLevel 将 slog 级别（数值越小越详细）映射为 logrus 级别。
// 两套级别数值体系完全不同，必须按值域比较。
func loggerToLogrusLevel(level logger.Level) logrus.Level {
	switch {
	case level <= logger.Trace:
		return logrus.TraceLevel
	case level <= logger.Debug:
		return logrus.DebugLevel
	case level <= logger.Info:
		return logrus.InfoLevel
	case level <= logger.Warn:
		return logrus.WarnLevel
	case level <= logger.Error:
		return logrus.ErrorLevel
	default:
		return logrus.FatalLevel
	}
}
