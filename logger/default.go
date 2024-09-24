package logger

import (
	"fmt"
	"io"
	"maps"
	"sync"
	"time"
)

var (
	DefaultLogger = New(&defaultLogger{fields: make(map[string]any)})
)

type defaultLogger struct {
	out    io.Writer
	fields map[string]any
	level  Level
	sync.RWMutex
}

func (l *defaultLogger) Init(opt Options) {
	l.level = opt.Level
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
		out:    l.out,
		level:  l.level,
		fields: nfields,
	}
}

func (l *defaultLogger) Log(level Level, args ...any) {
	message := fmt.Sprint(args...)
	l.write(level, message)
}

func (l *defaultLogger) Logf(level Level, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	l.write(level, message)
}

func (l *defaultLogger) write(level Level, message string) {
	t := time.Now().Format("2006-01-02 15:04:05")
	cloned := l.cloneFields()
	metadata := make([]string, 0, len(cloned))
	for k, v := range cloned {
		metadata = append(metadata, fmt.Sprintf("%s:%v", k, v))
	}

	fmt.Fprintf(l.out, "%s [%s] %s %v\n", t, level.String(), metadata, message)
}

func (*defaultLogger) String() string { return "default" }

func (l *defaultLogger) cloneFields() map[string]any {
	l.Lock()
	nfields := maps.Clone(l.fields)
	l.Unlock()

	return nfields
}
