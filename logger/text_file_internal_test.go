package logger

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newTextHandlerForTest 构造一个仅输出到内存缓冲、级别放行到 Info 的文件 text handler。
func newTextHandlerForTest(buf *bytes.Buffer, addSource bool) *fileTextHandler {
	return newFileTextHandler(buf, &FileTextOptions{Level: slog.LevelInfo, AddSource: addSource}).(*fileTextHandler)
}

// TestFileTextHandlerFieldOrder：注入含 trace_id/req_id 的 record（经 TraceHandler 包一层），
// 断言两字段出现在 msg 之前，且 msg 之后不再重复出现。
func TestFileTextHandlerFieldOrder(t *testing.T) {
	var buf bytes.Buffer
	th := NewTraceHandler(newTextHandlerForTest(&buf, false))

	ctx := WithReqID(WithTraceID(context.Background(), "trace-abc"), "req-xyz")
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello world", 0)
	r.AddAttrs(slog.String("user", "bob"))
	if err := th.Handle(ctx, r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	msgIdx := strings.Index(line, "msg=")
	traceIdx := strings.Index(line, "trace_id=")
	reqIdx := strings.Index(line, "req_id=")
	if msgIdx < 0 || traceIdx < 0 || reqIdx < 0 {
		t.Fatalf("expected trace_id/req_id/msg present, got: %s", line)
	}
	if traceIdx >= msgIdx || reqIdx >= msgIdx {
		t.Errorf("expected trace_id/req_id before msg, got: %s", line)
	}
	// trace_id 在 req_id 之前，user=bob 在 msg 之后
	if traceIdx >= reqIdx {
		t.Errorf("expected trace_id before req_id, got: %s", line)
	}
	userIdx := strings.Index(line, "user=")
	if userIdx <= msgIdx {
		t.Errorf("expected regular attr after msg, got: %s", line)
	}
	// 去重：msg 之后不得再出现 trace_id / req_id
	afterMsg := line[msgIdx:]
	if strings.Contains(afterMsg, "trace_id=") || strings.Contains(afterMsg, "req_id=") {
		t.Errorf("trace/req duplicated after msg: %s", afterMsg)
	}
}

// TestFileTextHandlerNoTrace：ctx 未注入 trace/req 时，不输出这两个 key。
func TestFileTextHandlerNoTrace(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandlerForTest(&buf, false)

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "plain", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	line := buf.String()
	if strings.Contains(line, "trace_id") || strings.Contains(line, "req_id") {
		t.Errorf("expected no trace/req keys, got: %s", line)
	}
}

// TestFileTextHandlerQuoting：msg 含空格时输出 msg="..."，纯单词时 msg=word 原样。
func TestFileTextHandlerQuoting(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandlerForTest(&buf, false)

	r1 := slog.NewRecord(time.Now(), slog.LevelInfo, "含 空格", 0)
	if err := h.Handle(context.Background(), r1); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); !strings.Contains(got, `msg="含 空格"`) {
		t.Errorf("expected quoted msg for space, got: %s", got)
	}

	buf.Reset()
	r2 := slog.NewRecord(time.Now(), slog.LevelInfo, "word", 0)
	if err := h.Handle(context.Background(), r2); err != nil {
		t.Fatalf("handle: %v", err)
	}
	line2 := strings.TrimSpace(buf.String())
	if !strings.Contains(line2, "msg=word") || strings.Contains(line2, `msg="word"`) {
		t.Errorf("expected unquoted msg for plain word, got: %s", line2)
	}
}

// TestFileTextHandlerAttrValueQuoting：字符串属性值含空格时同样加引号；非字符串值原样。
func TestFileTextHandlerAttrValueQuoting(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandlerForTest(&buf, false)

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "m", 0)
	r.AddAttrs(slog.String("path", "a b"), slog.Int("n", 5))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	if !strings.Contains(line, `path="a b"`) {
		t.Errorf("expected quoted string attr value, got: %s", line)
	}
	if !strings.Contains(line, "n=5") {
		t.Errorf("expected int attr value, got: %s", line)
	}
}

// TestFileTextHandlerTimeFormat：时间戳格式固定 "2006-01-02 15:04:05.000"，位于行首。
func TestFileTextHandlerTimeFormat(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandlerForTest(&buf, false)

	ts := time.Date(2026, 9, 8, 13, 5, 7, 123_000_000, time.UTC)
	r := slog.NewRecord(ts, slog.LevelInfo, "msg", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	re := regexp.MustCompile(`^time=2026-09-08 13:05:07\.123 level=`)
	if !re.MatchString(line) {
		t.Errorf("expected time format at line start, got: %s", line)
	}
}

// TestFileTextHandlerAddSource：AddSource=true 时含 source= 且位于 msg 之后。
func TestFileTextHandlerAddSource(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandlerForTest(&buf, true)

	pc, _, _, ok := runtime.Caller(0)
	if !ok {
		pc = 0
	}
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "src msg", pc)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	msgIdx := strings.Index(line, "msg=")
	srcIdx := strings.Index(line, "source=")
	if srcIdx < 0 {
		t.Fatalf("expected source= when AddSource, got: %s", line)
	}
	if msgIdx < 0 || srcIdx <= msgIdx {
		t.Errorf("expected source after msg, got: %s", line)
	}
}

// TestFileTextHandlerWithAttrsBeforeRecord：WithAttrs 累积属性排在 record 自身属性之前。
func TestFileTextHandlerWithAttrsBeforeRecord(t *testing.T) {
	var buf bytes.Buffer
	base := newTextHandlerForTest(&buf, false)
	h := base.WithAttrs([]slog.Attr{slog.String("preset", "p1")})

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "m", 0)
	r.AddAttrs(slog.String("later", "l1"))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	pIdx := strings.Index(line, "preset=")
	lIdx := strings.Index(line, "later=")
	if pIdx < 0 || lIdx <= pIdx {
		t.Errorf("expected WithAttrs before record attrs, got: %s", line)
	}
}

// TestFileTextHandlerGroupPrefix：WithGroup 与 record 内 Group 递归展开，键带点分前缀。
func TestFileTextHandlerGroupPrefix(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandlerForTest(&buf, false).WithGroup("cfg")

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "m", 0)
	r.AddAttrs(slog.Group("db", slog.String("host", "localhost"), slog.Int("port", 5432)))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	if !strings.Contains(line, "cfg.db.host=localhost") || !strings.Contains(line, "cfg.db.port=5432") {
		t.Errorf("expected group-prefixed attrs, got: %s", line)
	}
}
