package logger

import "fmt"

type Level int8

const (
	Trace Level = iota - 1
	Debug
	Info
	Warn
	Error
	Fatal
)

func (l Level) String() string {
	switch l {
	case Trace:
		return "trace"
	case Debug:
		return "debug"
	case Info:
		return "info"
	case Warn:
		return "warn"
	case Error:
		return "error"
	case Fatal:
		return "fatal"
	}
	return ""
}

func (l Level) Enabled(lvl Level) bool { return lvl > l }

func GetLevel(levelStr string) (Level, error) {
	switch levelStr {
	case Trace.String():
		return Trace, nil
	case Debug.String():
		return Debug, nil
	case Info.String():
		return Info, nil
	case Warn.String():
		return Warn, nil
	case Error.String():
		return Error, nil
	case Fatal.String():
		return Fatal, nil
	}

	return Info, fmt.Errorf("unknown Level String: '%s', defaulting to InfoLevel", levelStr)
}
