package logger

import "os"

type Helper struct {
	opt      Options
	recorder LogRecorder
}

func newHelper(opt Options, logger LogRecorder) Logger {
	return &Helper{opt: opt, recorder: logger}
}

func (h *Helper) Log(level Level, args ...any) {
	h.recorder.Log(level, args...)
}

func (h *Helper) Logf(level Level, format string, args ...any) {
	h.recorder.Logf(level, format, args...)
}

func (h *Helper) Info(args ...interface{}) {
	if !h.opt.Level.Enabled(Info) {
		return
	}
	h.recorder.Log(Info, args...)
}

func (h *Helper) Infof(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Info) {
		return
	}
	h.recorder.Logf(Info, template, args...)
}

func (h *Helper) Trace(args ...interface{}) {
	if !h.opt.Level.Enabled(Trace) {
		return
	}
	h.recorder.Log(Trace, args...)
}

func (h *Helper) Tracef(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Trace) {
		return
	}
	h.recorder.Logf(Trace, template, args...)
}

func (h *Helper) Debug(args ...interface{}) {
	if !h.opt.Level.Enabled(Debug) {
		return
	}
	h.recorder.Log(Debug, args...)
}

func (h *Helper) Debugf(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Debug) {
		return
	}
	h.recorder.Logf(Debug, template, args...)
}

func (h *Helper) Warn(args ...interface{}) {
	if !h.opt.Level.Enabled(Warn) {
		return
	}
	h.recorder.Log(Warn, args...)
}

func (h *Helper) Warnf(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Warn) {
		return
	}
	h.recorder.Logf(Warn, template, args...)
}

func (h *Helper) Error(args ...interface{}) {
	if !h.opt.Level.Enabled(Error) {
		return
	}
	h.recorder.Log(Error, args...)
}

func (h *Helper) Errorf(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Error) {
		return
	}
	h.recorder.Logf(Error, template, args...)
}

func (h *Helper) Fatal(args ...interface{}) {
	if !h.opt.Level.Enabled(Fatal) {
		return
	}
	h.recorder.Log(Fatal, args...)
	os.Exit(1)
}

func (h *Helper) Fatalf(template string, args ...interface{}) {
	if !h.opt.Level.Enabled(Fatal) {
		return
	}
	h.recorder.Logf(Fatal, template, args...)
	os.Exit(1)
}
