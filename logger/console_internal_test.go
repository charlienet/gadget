package logger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- ② console 格式化：appendValue 全 Kind 分发（直接调用，精准断言） ---

type consStringer struct{}

func (consStringer) String() string { return "i-am-stringer" }

type badMarshaler struct{}

func (badMarshaler) MarshalText() ([]byte, error) { return nil, errors.New("marshal failed") }

type textType struct{}

func (textType) MarshalText() ([]byte, error) { return []byte("TEXT-OK"), nil }

func TestAppendValueAllKinds(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.UTC)

	tests := []struct {
		name string
		val  slog.Value
		want string
	}{
		{"string", slog.StringValue("hello"), "hello"},
		{"bool", slog.BoolValue(true), "true"},
		{"bool-false", slog.BoolValue(false), "false"},
		{"int64", slog.Int64Value(-42), "-42"},
		{"uint64", slog.Uint64Value(42), "42"},
		{"float64", slog.Float64Value(2.5), "2.5"},
		{"duration", slog.DurationValue(1500 * time.Millisecond), "1.5s"},
		{"time", slog.TimeValue(fixed), "2026-01-02 03:04:05.600"},
		// KindGroup 直接进 appendValue（appendAttr 会拦截展开，此处覆盖 default 兜底分支）
		{"group", slog.GroupValue(slog.String("a", "b"), slog.Int64("c", 1)), "{a=b c=1}"},
		{"any-struct", slog.AnyValue(struct{ X int }{7}), "{X:7}"},
		{"any-map", slog.AnyValue(map[string]int{"k": 1}), "map[k:1]"},
		{"any-slice", slog.AnyValue([]int{1, 2}), "[1 2]"},
		{"any-error", slog.AnyValue(errors.New("boom")), "boom"},
		{"any-nil", slog.AnyValue(nil), "<nil>"},
		{"any-stringer", slog.AnyValue(consStringer{}), "i-am-stringer"},
		// encoding.TextMarshaler 优先于 %+v（time.Time 会被识别为 KindTime，此处用非特殊类型）
		{"any-textmarshaler", slog.AnyValue(textType{}), "TEXT-OK"},
		// TextMarshaler 失败回退 %+v
		{"any-badmarshaler", slog.AnyValue(badMarshaler{}), "{}"},
		// KindLogValuer 直接进 appendValue（appendAttr/appendTextValue 均已先 Resolve，
		// 此处覆盖 default 兜底防御分支：Resolve 后按 Group 渲染，不 panic）
		{"logvaluer-defensive", slog.AnyValue(logValUser{Name: "bob", Age: 30}), "{name=bob age=30}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(appendValue(nil, tt.val))
			if got != tt.want {
				t.Errorf("appendValue(%v) = %q, want %q", tt.val, got, tt.want)
			}
		})
	}
}

// TestAppendTextValueResolvesLogValuerString：直调 appendTextValue 断言
// LogValuer（Kind==KindLogValuer）在入口 Resolve 后按 KindString 走 quoting 规则
// （golden 含引号）。判别力锁定：删除 appendTextValue 入口的 v.Resolve() 后，
// KindLogValuer ≠ KindString 漏进 appendValue 的 KindAny 分支、绕过 appendQuoted
// 输出无引号 "a b"，本用例必红。
func TestAppendTextValueResolvesLogValuerString(t *testing.T) {
	got := string(appendTextValue(nil, slog.AnyValue(logValStr{})))
	if got != `"a b"` {
		t.Errorf("appendTextValue(AnyValue(logValStr{})) = %q, want %q", got, `"a b"`)
	}
}

// --- ② levelColor / formatLevel 全分支 ---

func TestLevelColorAndFormat(t *testing.T) {
	// formatLevel/levelColor 用 `<=` 边界判定，6 个代表级别覆盖全部分支
	tests := []struct {
		level     slog.Level
		wantText  string
		wantColor string
	}{
		{Trace, "TRAC", colorTrace},
		{Debug, "DEBU", colorDebug},
		{Info, "INFO", colorInfo},
		{Warn, "WARN", colorWarn},
		{Error, "ERRO", colorError},
		{FatalLevel, "FATA", colorFatal},
	}

	for _, tt := range tests {
		if got := formatLevel(tt.level); got != tt.wantText {
			t.Errorf("formatLevel(%v) = %q, want %q", tt.level, got, tt.wantText)
		}
		if got := levelColor(tt.level); got != tt.wantColor {
			t.Errorf("levelColor(%v) = %q, want %q", tt.level, got, tt.wantColor)
		}
	}
}

// --- ② NewConsoleHandler 公开构造器入口 ---

func TestNewConsoleHandlerPublic(t *testing.T) {
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &ConsoleOptions{Level: slog.LevelDebug, NoColor: true})
	lg := slog.New(h)

	lg.Debug("pub debug", "k", "v")
	lg.Info("pub info")
	got := buf.String()
	if !strings.Contains(got, "pub debug") || !strings.Contains(got, "k=v") {
		t.Errorf("expected debug with attrs, got: %s", got)
	}
	if !strings.Contains(got, "INFO pub info") {
		t.Errorf("expected info line, got: %s", got)
	}

	// 级别过滤：Warn 阈值下 Debug 无输出
	var buf2 bytes.Buffer
	lg2 := slog.New(NewConsoleHandler(&buf2, &ConsoleOptions{Level: slog.LevelWarn, NoColor: true}))
	lg2.Debug("hidden")
	if buf2.Len() > 0 {
		t.Errorf("expected Debug filtered at Warn, got: %s", buf2.String())
	}
	lg2.Warn("shown")
	if !strings.Contains(buf2.String(), "shown") {
		t.Errorf("expected Warn output, got: %s", buf2.String())
	}

	// 彩色模式：key 上色 + group 前缀（appendAttr 彩色分支）
	// 注意彩色输出中 ANSI reset 夹在 key= 与 value 之间，故分开断言子串
	var buf3 bytes.Buffer
	lg3 := slog.New(NewConsoleHandler(&buf3, &ConsoleOptions{Level: slog.LevelInfo, NoColor: false}))
	lg3.WithGroup("g").Info("color msg", "key", "val")
	got3 := buf3.String()
	if !strings.Contains(got3, colorKey) || !strings.Contains(got3, "g.key=") || !strings.Contains(got3, "val") {
		t.Errorf("expected colored prefixed key, got: %q", got3)
	}
}

// --- consoleHandler 派生 no-op 分支（WithAttrs 空 / WithGroup 空） ---

func TestConsoleHandlerDerivedNoops(t *testing.T) {
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &ConsoleOptions{Level: slog.LevelInfo, NoColor: true}).(*consoleHandler)

	if got := h.WithAttrs(nil); got != h {
		t.Error("expected WithAttrs(nil) to return same handler")
	}
	if got := h.WithGroup(""); got != h {
		t.Error("expected WithGroup(\"\") to return same handler")
	}
	// 非空派生是新实例且携带属性（共享 writer 与互斥锁，writer 构造后固定）
	dh := h.WithAttrs([]slog.Attr{slog.String("pre", "set")}).(*consoleHandler)
	if dh == h {
		t.Error("expected new handler from WithAttrs")
	}
	_ = dh.Handle(context.Background(), slog.NewRecord(time.Now(), Info, "m", 0))
	if !strings.Contains(buf.String(), "pre=set") {
		t.Errorf("expected preset attr in derived output, got: %s", buf.String())
	}
}

// --- sourceFromPC 无效 PC 分支 ---

func TestSourceFromPCInvalid(t *testing.T) {
	if src, ok := sourceFromPC(0); ok || src != "" {
		t.Errorf("expected (\"\", false) for pc=0, got (%q, %v)", src, ok)
	}
	// 有效 PC：当前函数调用点
	pc := make([]uintptr, 1)
	n := runtime.Callers(1, pc)
	if n == 0 {
		t.Skip("no caller pc available")
	}
	src, ok := sourceFromPC(pc[0])
	if !ok || !strings.Contains(src, "console_internal_test.go:") {
		t.Errorf("expected file:line of this test, got (%q, %v)", src, ok)
	}
}

// --- TraceHandler 边界：nil ctx 透传 / 错误类型 value 忽略 ---

func TestTraceHandlerEdges(t *testing.T) {
	var buf bytes.Buffer
	th := NewTraceHandler(slog.NewTextHandler(&buf, nil))

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "nil ctx msg", 0)
	if err := th.Handle(nil, r); err != nil { //nolint:staticcheck // 故意验证 nil ctx 防御分支行为
		t.Fatalf("Handle(nil ctx): %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "nil ctx msg") || strings.Contains(got, "trace_id") {
		t.Errorf("expected passthrough without trace attrs, got: %s", got)
	}

	// ctx 中 key 类型正确但 value 非 string：提取回退空串，不注入
	ctx := context.WithValue(context.Background(), traceIDKey{}, 42)
	ctx = context.WithValue(ctx, reqIDKey{}, struct{}{})
	if GetTraceID(ctx) != "" || GetReqID(ctx) != "" {
		t.Error("expected empty ids for non-string values")
	}
	buf.Reset()
	r2 := slog.NewRecord(time.Now(), slog.LevelInfo, "wrong type", 0)
	if err := th.Handle(ctx, r2); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("expected no trace_id for wrong value type, got: %s", buf.String())
	}
}

// --- GetTraceID/GetReqID nil ctx 分支 ---

func TestTraceGettersNilCtx(t *testing.T) {
	if GetTraceID(nil) != "" || GetReqID(nil) != "" { //nolint:staticcheck // 故意验证 nil ctx 防御分支行为
		t.Error("expected empty ids for nil ctx")
	}
}

// --- buildFileWriter 两条构建路径 ---

func TestBuildFileWriterSizeMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "size.log")

	w := buildFileWriter(path, nil) // nil FileOpts → 默认配置 + lumberjack
	if w == nil {
		t.Fatal("expected non-nil writer")
	}
	defer func() {
		_ = w.Close()
	}()

	if _, err := w.Write([]byte("size mode line\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "size mode line") {
		t.Errorf("expected size-mode content, got %q err=%v", data, err)
	}
}

func TestBuildFileWriterDateMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "date.log")

	w := buildFileWriter(path, &FileOptions{Layout: "2006-01-02", Compress: false})
	if w == nil {
		t.Fatal("expected non-nil writer")
	}
	if _, err := w.Write([]byte("date mode line\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 日期模式：真实文件名带日期后缀
	want := filepath.Join(dir, fmt.Sprintf("date.%s.log", time.Now().Format("2006-01-02")))
	data, err := os.ReadFile(want)
	_ = w.Close()
	if err != nil || !strings.Contains(string(data), "date mode line") {
		t.Errorf("expected date-mode content at %s, got %q err=%v", want, data, err)
	}
}

// --- Sampling 构造器归一化分支 ---

func TestNewSamplingHandlerNormalization(t *testing.T) {
	// opts == nil：first=0, thereafter=1 → 全部透传（无过滤效果但可用）
	var buf bytes.Buffer
	h := NewSamplingHandler(slog.NewTextHandler(&buf, nil), nil)
	for range 3 {
		if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), Info, "nil opts", 0)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	if got := strings.Count(buf.String(), "nil opts"); got != 3 {
		t.Errorf("nil opts: expected 3 lines, got %d", got)
	}

	// First<0 → 0；Thereafter<=0 → 1：等价全放行
	buf.Reset()
	h2 := NewSamplingHandler(slog.NewTextHandler(&buf, nil), &SamplingOptions{First: -5, Thereafter: 0})
	for range 4 {
		_ = h2.Handle(context.Background(), slog.NewRecord(time.Now(), Info, "neg", 0))
	}
	if got := strings.Count(buf.String(), "neg"); got != 4 {
		t.Errorf("normalized handler: expected 4 lines, got %d", got)
	}

	// WithAttrs(nil) / WithGroup("") no-op 分支
	if h2.WithAttrs(nil) != h2 {
		t.Error("expected WithAttrs(nil) same instance")
	}
	if h2.WithGroup("") != h2 {
		t.Error("expected WithGroup(\"\") same instance")
	}
}

// --- Sensitive 构造器 opts==nil 路径 ---
func TestNewSensitiveHandlerNilOpts(t *testing.T) {
	var buf bytes.Buffer
	h := NewSensitiveHandler(slog.NewTextHandler(&buf, nil), nil)

	r := slog.NewRecord(time.Now(), Info, "m", 0)
	r.AddAttrs(slog.String("token", "raw-secret"))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "raw-secret") || !strings.Contains(got, "******") {
		t.Errorf("expected builtin mask with nil opts, got: %s", got)
	}

	// 派生 no-op 分支
	if h.WithAttrs(nil) != h || h.WithGroup("") != h {
		t.Error("expected nil/empty derive to return same handler")
	}
}

// --- maskText 空关键词分支 ---

func TestMaskTextEmptyKeyword(t *testing.T) {
	if got := maskText("abcdef", []string{""}, "*"); got != "abcdef" {
		t.Errorf("empty keyword must not alter text, got: %s", got)
	}
}

// --- registerLogger / unregisterLogger 命中与未命中分支 ---

func TestRegistryRegisterUnregister(t *testing.T) {
	fake := &slogLogger{}
	registerLogger(fake)

	found := false
	loggerMu.Lock()
	for _, v := range loggerList {
		if v == fake {
			found = true
		}
	}
	loggerMu.Unlock()
	if !found {
		t.Fatal("expected fake registered")
	}

	unregisterLogger(fake)          // 命中删除分支
	unregisterLogger(&slogLogger{}) // 未命中分支（不在表内，不 panic）

	loggerMu.Lock()
	for _, v := range loggerList {
		if v == fake {
			loggerMu.Unlock()
			t.Fatal("expected fake unregistered")
		}
	}
	loggerMu.Unlock()
}

// --- stackHandler 空派生 no-op 分支 ---

func TestStackHandlerDerivedNoops(t *testing.T) {
	h := NewStackHandler(slog.NewTextHandler(io.Discard, nil))
	if got := h.WithAttrs(nil); got != h {
		t.Error("expected stackHandler WithAttrs(nil) same instance")
	}
	if got := h.WithGroup(""); got != h {
		t.Error("expected stackHandler WithGroup(\"\") same instance")
	}
}

// --- attrHasSensitive：组内全非敏感（遍历完整返回 false 分支） ---

func TestSensitiveGroupAllClean(t *testing.T) {
	var buf bytes.Buffer
	h := NewSensitiveHandler(slog.NewTextHandler(&buf, nil), &SensitiveOptions{})
	r := slog.NewRecord(time.Now(), Info, "clean", 0)
	r.AddAttrs(slog.Group("meta", slog.String("region", "cn"), slog.String("env", "dev")))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "******") {
		t.Errorf("expected no mask for clean group, got: %s", got)
	}
	if !strings.Contains(got, "region=cn") {
		t.Errorf("expected clean attrs kept, got: %s", got)
	}
}

// --- slogLogger.close：fileCloser 出错向上传播 ---

// errCloserError sentinel：供 errCloser.Close() 与跨文件测试共享同一实例，
// 使 errors.Is 能跨文件匹配（errors.New 每次返回不同实例，errors.Is 无法跨调用匹配）。
var errCloserError = errors.New("closer boom")

type errCloser struct{}

func (errCloser) Close() error { return errCloserError }

func TestSlogLoggerCloseErrorPropagation(t *testing.T) {
	sl := &slogLogger{fileCloser: errCloser{}}
	if err := sl.close(0); err == nil || !strings.Contains(err.Error(), "closer boom") {
		t.Errorf("expected fileCloser error propagated, got: %v", err)
	}
	// closeOnce 幂等：第二次调用不再触发错误
	if err := sl.close(0); err != nil {
		t.Errorf("expected idempotent close, got: %v", err)
	}
	// 已注销（close 内部 unregister），未注册实例的注销走未命中分支
	unregisterLogger(sl)
}

// --- console 字段顺序：trace_id/req_id 前置到 msg 之前、其余 attrs 去重 ---

// TestConsoleHandlerTraceFieldOrder：注入带 trace_id/req_id 的 record（经 TraceHandler 包一层），
// 断言两字段以裸值形态出现在消息之前、消息之后不重复；并核对 console 观感
// （与 fileText 共享同一裸值版式：级别为裸词 INFO、前置段无 key= 前缀）。
func TestConsoleHandlerTraceFieldOrder(t *testing.T) {
	var buf bytes.Buffer
	th := NewTraceHandler(NewConsoleHandler(&buf, &ConsoleOptions{Level: slog.LevelInfo, NoColor: true}))

	ctx := WithReqID(WithTraceID(context.Background(), "trace-abc"), "req-xyz")
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello world", 0)
	r.AddAttrs(slog.String("user", "bob"))
	if err := th.Handle(ctx, r); err != nil {
		t.Fatalf("handle: %v", err)
	}
	line := strings.TrimSpace(buf.String())

	traceIdx := strings.Index(line, " trace-abc")
	reqIdx := strings.Index(line, " req-xyz")
	msgIdx := strings.Index(line, "hello world")
	userIdx := strings.Index(line, "user=bob")
	if traceIdx < 0 || reqIdx < 0 || msgIdx < 0 || userIdx < 0 {
		t.Fatalf("expected trace/req bare values/msg/user present, got: %q", line)
	}
	if traceIdx >= reqIdx || reqIdx >= msgIdx || msgIdx >= userIdx {
		t.Errorf("expected order trace_id<req_id<msg<user, got: %q", line)
	}
	// 去重：消息之后不得再出现 trace/req 值，也不得回退成 key= 形态
	if afterMsg := line[msgIdx:]; strings.Contains(afterMsg, "trace-abc") || strings.Contains(afterMsg, "req-xyz") {
		t.Errorf("trace/req duplicated after msg: %q", afterMsg)
	}
	// console 观感：级别为裸词（无方括号）、前置段裸值无 key= 前缀
	if !strings.Contains(line, "INFO trace-abc req-xyz") {
		t.Errorf("expected bare level then bare front values, got: %q", line)
	}
	if strings.Contains(line, "trace_id=") || strings.Contains(line, "req_id=") || strings.Contains(line, "[INFO]") {
		t.Errorf("expected no key= prefix on front fields and no bracketed level, got: %q", line)
	}
}

// TestConsoleHandlerTraceFieldColor：彩色分支——ANSI 仅叠加于时间/级别词，前置字段为裸值
// 且不上色不加引号（与 fileText 共享同一裸值版式，颜色只是附加层）。
func TestConsoleHandlerTraceFieldColor(t *testing.T) {
	var buf bytes.Buffer
	th := NewTraceHandler(NewConsoleHandler(&buf, &ConsoleOptions{Level: slog.LevelInfo, NoColor: false}))

	ctx := WithTraceID(context.Background(), "trace-abc")
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "color trace", 0)
	if err := th.Handle(ctx, r); err != nil {
		t.Fatalf("handle: %v", err)
	}
	got := buf.String()
	// 级别词按级染色：... <colorInfo>INFO<reset> <裸值 trace-abc
	if !strings.Contains(got, colorInfo+"INFO"+colorReset+" trace-abc") {
		t.Errorf("expected colored bare level word then uncolored unquoted front value, got: %q", got)
	}
	// 时间亮白包裹
	if !strings.Contains(got, colorTimestamp) {
		t.Errorf("expected colored timestamp, got: %q", got)
	}
	// 前置字段不再有 key= 上色形态
	if strings.Contains(got, colorKey+"trace_id=") {
		t.Errorf("expected no colored key= for front fields, got: %q", got)
	}
}

// TestConsoleHandlerMessageNewlineEscape：msg 中 \n/\r 以字面量两字符（反斜杠+字母）
// 转义保持单行——防按 req_id/trace_id grep 过滤时断行；不加引号，\t 等其余字节原样。
func TestConsoleHandlerMessageNewlineEscape(t *testing.T) {
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &ConsoleOptions{Level: slog.LevelInfo, NoColor: true})

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "line1\nline2\r\nx\ty", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}
	got := buf.String()

	// 整条日志仅占一行：全输出只允许行尾那一个真实 '\n'
	if strings.Count(got, "\n") != 1 {
		t.Errorf("expected single line (one trailing newline), got %d newlines: %q", strings.Count(got, "\n"), got)
	}
	// 转义后字面量：反斜杠n / 反斜杠r；tab 原样保留；不加引号
	line := strings.TrimSpace(got)
	if !strings.Contains(line, `line1\nline2\r\nx`+"\ty") {
		t.Errorf("expected escaped-literal \\n \\r and raw tab in msg, got: %q", line)
	}
	if strings.Contains(line, `"line1`) || strings.Contains(line, `y"`) {
		t.Errorf("msg must not be wrapped in quotes, got: %q", line)
	}
}

// TestConsoleHandlerNoTrace：ctx 未注入 trace/req 时，不输出这两个 key。
func TestConsoleHandlerNoTrace(t *testing.T) {
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &ConsoleOptions{Level: slog.LevelInfo, NoColor: true})

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "plain", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}
	line := buf.String()
	if strings.Contains(line, "trace_id") || strings.Contains(line, "req_id") {
		t.Errorf("expected no trace/req keys, got: %q", line)
	}
}
