package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

// ExitFunc is called by Fatal/Fatalf after logging.
// Override in tests to prevent process termination.
var ExitFunc = os.Exit

type loggerHelper struct {
	mu         sync.RWMutex
	opt        Options
	recorder   LogRecorder
	group      string    // 分组前缀（如 "g."），不可变，派生时携带
	fileWriter io.Writer // 文件输出 writer，nil = 无文件输出
}

func newHelper(opt Options, logger LogRecorder, fw io.Writer) Logger {
	return &loggerHelper{opt: opt, recorder: logger, fileWriter: fw}
}

func (h *loggerHelper) WithField(key string, value any) Logger {
	return h.WithFields(map[string]any{key: value})
}

func (h *loggerHelper) WithFields(fields map[string]any) Logger {
	h.mu.RLock()
	opt := h.opt
	h.mu.RUnlock()

	// key 统一加 group 前缀，保证分组语义一致（无 group 时行为不变）
	if h.group != "" {
		prefixed := make(map[string]any, len(fields))
		for k, v := range fields {
			prefixed[h.group+k] = v
		}
		fields = prefixed
	}

	r := h.recorder.Fields(fields)
	return &loggerHelper{opt: opt, recorder: r, group: h.group, fileWriter: h.fileWriter}
}

// WithAttrs 追加 slog.Attr 风格字段。
// 转 map 后复用 WithFields，key 前缀统一由 WithFields 处理，避免双重前缀。
func (h *loggerHelper) WithAttrs(attrs ...slog.Attr) Logger {
	return h.WithFields(attrsToMap("", attrs))
}

// WithGroup 分组：返回 group 为 h.group+name+"." 的新 helper（recorder/opt 复用）
func (h *loggerHelper) WithGroup(name string) Logger {
	h.mu.RLock()
	opt := h.opt
	h.mu.RUnlock()

	return &loggerHelper{opt: opt, recorder: h.recorder, group: h.group + name + ".", fileWriter: h.fileWriter}
}

// LogAttrs 记录一条 slog.Attr 风格日志（映射为 LogRecorder 字段后输出）
func (h *loggerHelper) LogAttrs(level slog.Level, msg string, attrs ...slog.Attr) {
	h.mu.RLock()
	rec := h.recorder
	group := h.group
	h.mu.RUnlock()

	rec.Fields(attrsToMap(group, attrs)).Log(level, msg)
}

// attrsToMap 将 slog.Attr 列表转为 map（value 用 a.Value.Any()，map 无 Kind 概念）。
// prefix 非空时 key 为 prefix+key（如 "g."+"k"="g.k"）。
// KindGroup 递归展开：group 内 attr 的 key 为 prefix+"group."+key（与 WithGroup 前缀语义一致）。
func attrsToMap(prefix string, attrs []slog.Attr) map[string]any {
	if len(attrs) == 0 {
		return nil
	}

	m := make(map[string]any, len(attrs))
	for _, a := range attrs {
		if a.Value.Kind() == slog.KindGroup {
			inner := a.Value.Group()
			for k, v := range attrsToMap(prefix+a.Key+".", inner) {
				m[k] = v
			}
			continue
		}

		key := a.Key
		if prefix != "" {
			key = prefix + key
		}
		m[key] = a.Value.Any()
	}

	return m
}

// With 按 slog 风格追加字段（成对 key/value，奇数个时按 slog 规则补 !BADKEY）
func (h *loggerHelper) With(args ...any) Logger {
	if len(args)%2 != 0 {
		args = append(args, "!BADKEY")
	}

	fields := make(map[string]any, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		k, ok := args[i].(string)
		if !ok {
			k = fmt.Sprint(args[i])
		}
		fields[k] = args[i+1]
	}

	return h.WithFields(fields)
}

// Log 记录一条 slog 风格日志。
// args 不再静默丢弃：按 key=value 对转为 map 后合并进 recorder 字段
// （奇数个时按 slog 规则补 !BADKEY），与 LogAttrs 行为一致。
func (h *loggerHelper) Log(level slog.Level, msg string, args ...any) {
	h.mu.RLock()
	rec := h.recorder
	group := h.group
	h.mu.RUnlock()

	rec.Fields(attrsToMap(group, argsToAttrs(args))).Log(level, msg)
}

// argsToAttrs 将 slog 风格成对参数转为 []slog.Attr（奇数个时按 slog 规则补 !BADKEY；
// 非字符串 key 用 fmt.Sprint 转换）
func argsToAttrs(args []any) []slog.Attr {
	if len(args)%2 != 0 {
		args = append(args, "!BADKEY")
	}

	attrs := make([]slog.Attr, 0, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		k, ok := args[i].(string)
		if !ok {
			k = fmt.Sprint(args[i])
		}
		attrs = append(attrs, slog.Any(k, args[i+1]))
	}

	return attrs
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
	if h.fileWriter != nil {
		h.opt.Out = io.MultiWriter(out, h.fileWriter) // 重定向后文件输出仍生效
	}
	h.recorder.Init(h.opt)
	h.mu.Unlock()
}

// enabled 判断指定级别在当前配置下是否启用（slog 级别数值越大越严重）
func (h *loggerHelper) enabled(level Level) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return level >= h.opt.Level
}

func (h *loggerHelper) Info(args ...interface{}) {
	if h.enabled(Info) {
		h.recorder.Log(Info, args...)
	}
}

func (h *loggerHelper) Infof(template string, args ...interface{}) {
	if h.enabled(Info) {
		h.recorder.Logf(Info, template, args...)
	}
}

func (h *loggerHelper) Trace(args ...interface{}) {
	if h.enabled(Trace) {
		h.recorder.Log(Trace, args...)
	}
}

func (h *loggerHelper) Tracef(template string, args ...interface{}) {
	if h.enabled(Trace) {
		h.recorder.Logf(Trace, template, args...)
	}
}

func (h *loggerHelper) Debug(args ...interface{}) {
	if h.enabled(Debug) {
		h.recorder.Log(Debug, args...)
	}
}

func (h *loggerHelper) Debugf(template string, args ...interface{}) {
	if h.enabled(Debug) {
		h.recorder.Logf(Debug, template, args...)
	}
}

func (h *loggerHelper) Warn(args ...interface{}) {
	if h.enabled(Warn) {
		h.recorder.Log(Warn, args...)
	}
}

func (h *loggerHelper) Warnf(template string, args ...interface{}) {
	if h.enabled(Warn) {
		h.recorder.Logf(Warn, template, args...)
	}
}

func (h *loggerHelper) Error(args ...interface{}) {
	if h.enabled(Error) {
		h.recorder.Log(Error, args...)
	}
}

func (h *loggerHelper) Errorf(template string, args ...interface{}) {
	if h.enabled(Error) {
		h.recorder.Logf(Error, template, args...)
	}
}

func (h *loggerHelper) Fatal(args ...interface{}) {
	if h.enabled(Fatal) {
		h.recorder.Log(Fatal, args...)
		ExitFunc(1)
	}
}

func (h *loggerHelper) Fatalf(template string, args ...interface{}) {
	if h.enabled(Fatal) {
		h.recorder.Logf(Fatal, template, args...)
		ExitFunc(1)
	}
}

func (h *loggerHelper) WithContext(parent context.Context) context.Context {
	return WithLogger(parent, h)
}
