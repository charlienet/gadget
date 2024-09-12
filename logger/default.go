package logger

import (
	"io"
	"log"
	"maps"
	"sync"
)

var (
	DefaultLogger = New(&defaultLogger{})
)

type defaultLogger struct {
	out             io.Writer
	fields          map[string]any
	level           Level
	callerSkipCount int
	sync.RWMutex
}

func (l *defaultLogger) Init(opt Options) {
	l.out = opt.Out
}

func (l *defaultLogger) Fields(fields map[string]interface{}) LogRecorder {
	l.Lock()
	nfields := maps.Clone(l.fields)
	l.Unlock()

	for k, v := range fields {
		nfields[k] = v
	}

	return &defaultLogger{
		fields: nfields,
	}
}

func (l *defaultLogger) Log(level Level, args ...any) {
	log.Print(args...)
}

func (l *defaultLogger) Logf(level Level, format string, args ...any) {
	log.Printf(format, args...)
}

func (*defaultLogger) String() string { return "default" }
