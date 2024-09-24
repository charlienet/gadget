package logger

import "io"

type Logger interface {
	WithField(name string, sss any) Logger
	WithFields(field map[string]any) Logger
	SetLevel(lvl Level)
	SetOutput(out io.Writer)
	Info(args ...any)
	Infof(template string, args ...any)
	Trace(args ...any)
	Tracef(template string, args ...any)
	Debug(args ...any)
	Debugf(template string, args ...any)
	Warn(args ...any)
	Warnf(template string, args ...any)
	Error(args ...any)
	Errorf(template string, args ...any)
	Fatal(args ...any)
	Fatalf(template string, args ...any)
}

// LogRecorder is a generic logging interface.
type LogRecorder interface {

	// Init initializes options
	Init(options Options)

	// The Logger options
	// Options() Options
	// Fields set fields to always be logged
	Fields(fields map[string]any) LogRecorder

	// Log writes a log entry
	Log(level Level, v ...any)

	// Logf writes a formatted log entry
	Logf(level Level, format string, v ...any)

	// String returns the name of logger
	String() string
}
