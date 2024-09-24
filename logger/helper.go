package logger

import (
	"io"
	"os"
)

type loggerHelper struct {
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
	r := h.recorder.Fields(fields)
	return newHelper(h.opt, r)
}

func (h *loggerHelper) SetLevel(lvl Level) {
	h.opt.Level = lvl
	h.recorder.Init(h.opt)
}

func (h *loggerHelper) SetOutput(out io.Writer) {
	h.opt.Out = out
	h.recorder.Init(h.opt)
}

func (h *loggerHelper) Log(level Level, args ...any) {
	h.recorder.Log(level, args...)
}

func (h *loggerHelper) Logf(level Level, format string, args ...any) {
	h.recorder.Logf(level, format, args...)
}

func (h *loggerHelper) Info(args ...interface{}) {
	if !h.opt.Level.Enabled(Info) {
		return
	}
	h.recorder.Log(Info, args...)
}

func (h *loggerHelper) Infof(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Info) {
		return
	}
	h.recorder.Logf(Info, template, args...)
}

func (h *loggerHelper) Trace(args ...interface{}) {
	if !h.opt.Level.Enabled(Trace) {
		return
	}
	h.recorder.Log(Trace, args...)
}

func (h *loggerHelper) Tracef(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Trace) {
		return
	}
	h.recorder.Logf(Trace, template, args...)
}

func (h *loggerHelper) Debug(args ...interface{}) {
	if !h.opt.Level.Enabled(Debug) {
		return
	}
	h.recorder.Log(Debug, args...)
}

func (h *loggerHelper) Debugf(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Debug) {
		return
	}
	h.recorder.Logf(Debug, template, args...)
}

func (h *loggerHelper) Warn(args ...interface{}) {
	if !h.opt.Level.Enabled(Warn) {
		return
	}
	h.recorder.Log(Warn, args...)
}

func (h *loggerHelper) Warnf(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Warn) {
		return
	}
	h.recorder.Logf(Warn, template, args...)
}

func (h *loggerHelper) Error(args ...interface{}) {
	if !h.opt.Level.Enabled(Error) {
		return
	}
	h.recorder.Log(Error, args...)
}

func (h *loggerHelper) Errorf(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Error) {
		return
	}
	h.recorder.Logf(Error, template, args...)
}

func (h *loggerHelper) Fatal(args ...interface{}) {
	if !h.opt.Level.Enabled(Fatal) {
		return
	}
	h.recorder.Log(Fatal, args...)
	os.Exit(1)
}

func (h *loggerHelper) Fatalf(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Fatal) {
		return
	}
	h.recorder.Logf(Fatal, template, args...)
	os.Exit(1)
}
