package logrus

import (
	"github.com/charlienet/gadget/logger"
	"github.com/sirupsen/logrus"
)

var DefaultLogger = logger.New(New())

type entryLogger interface {
	WithFields(logrus.Fields) *logrus.Entry

	Log(level logrus.Level, args ...any)
	Logf(level logrus.Level, format string, args ...any)
}

type logrusLogger struct {
	Logger entryLogger
}

func New(opts ...option) logger.LogRecorder {
	opt := Options{Formatter: &logrus.TextFormatter{}}
	for _, o := range opts {
		o(&opt)
	}

	l := logrus.New()
	l.SetFormatter(opt.Formatter)

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

func setOptions(ll *logrus.Logger, opt logger.Options) {
	ll.SetLevel(loggerToLogrusLevel(opt.Level))
	ll.SetOutput(opt.Out)
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

func loggerToLogrusLevel(level logger.Level) logrus.Level {
	switch level {
	case logger.Trace:
		return logrus.TraceLevel
	case logger.Debug:
		return logrus.DebugLevel
	case logger.Info:
		return logrus.InfoLevel
	case logger.Warn:
		return logrus.WarnLevel
	case logger.Error:
		return logrus.ErrorLevel
	case logger.Fatal:
		return logrus.FatalLevel
	default:
		return logrus.InfoLevel
	}
}
