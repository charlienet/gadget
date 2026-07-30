package logger

import (
	"context"
	"io"
	"os"
	"sync"
)

// ExitFunc is called by Fatal/Fatalf after logging.
// Override in tests to prevent process termination.
var ExitFunc = os.Exit

type loggerHelper struct {
	mu       sync.RWMutex
	opt      Options
	recorder LogRecorder
}

func newHelper(opt Options, logger LogRecorder) Logger {
	return &loggerHelper{opt: opt, recorder: logger}
}

func (h *loggerHelper) WithField(key string, value any) Logger {
	return h.WithFields(map[string]any{key: value})
}

func (h *loggerHelper) WithFields(fields map[string]any) Logger {
	h.mu.RLock()
	opt := h.opt
	h.mu.RUnlock()

	r := h.recorder.Fields(fields)
	return newHelper(opt, r)
}

func (h *loggerHelper) SetLevel(lvl Level) {
	h.mu.Lock()
	h.opt.Level = lvl
	h.recorder.Init(h.opt)
	h.mu.Unlock()
}

func (h *loggerHelper) SetOutput(out io.Writer) {
	h.mu.Lock()
	h.opt.Out = out
	h.recorder.Init(h.opt)
	h.mu.Unlock()
}

func (h *loggerHelper) Log(level Level, args ...any) {
	h.recorder.Log(level, args...)
}

func (h *loggerHelper) Logf(level Level, format string, args ...any) {
	h.recorder.Logf(level, format, args...)
}

func (h *loggerHelper) Info(args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Info)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Log(Info, args...)
	}
}

func (h *loggerHelper) Infof(template string, args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Info)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Logf(Info, template, args...)
	}
}

func (h *loggerHelper) Trace(args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Trace)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Log(Trace, args...)
	}
}

func (h *loggerHelper) Tracef(template string, args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Trace)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Logf(Trace, template, args...)
	}
}

func (h *loggerHelper) Debug(args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Debug)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Log(Debug, args...)
	}
}

func (h *loggerHelper) Debugf(template string, args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Debug)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Logf(Debug, template, args...)
	}
}

func (h *loggerHelper) Warn(args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Warn)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Log(Warn, args...)
	}
}

func (h *loggerHelper) Warnf(template string, args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Warn)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Logf(Warn, template, args...)
	}
}

func (h *loggerHelper) Error(args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Error)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Log(Error, args...)
	}
}

func (h *loggerHelper) Errorf(template string, args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Error)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Logf(Error, template, args...)
	}
}

func (h *loggerHelper) Fatal(args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Fatal)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Log(Fatal, args...)
		ExitFunc(1)
	}
}

func (h *loggerHelper) Fatalf(template string, args ...interface{}) {
	h.mu.RLock()
	enabled := h.opt.Level.Enabled(Fatal)
	h.mu.RUnlock()
	if enabled {
		h.recorder.Logf(Fatal, template, args...)
		ExitFunc(1)
	}
}

func (h *loggerHelper) WithContext(parent context.Context) context.Context {
	return WithLogger(parent, h)
}
