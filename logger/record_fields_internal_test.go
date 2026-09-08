package logger

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// assertFieldOrder 断言输出行中各 key 的首次出现位置严格递增（用于跨 console/fileText
// 校验字段顺序一致，忽略各自的引号/括号/颜色差异）。
func assertFieldOrder(t *testing.T, line string, keys []string) {
	t.Helper()
	prev := -1
	for _, k := range keys {
		idx := strings.Index(line, k)
		if idx < 0 {
			t.Fatalf("expected key %q present in: %q", k, line)
		}
		if idx <= prev {
			t.Errorf("expected field order %v; key %q out of place in: %q", keys, k, line)
		}
		prev = idx
	}
}

// TestConsoleAndFileTextShareFieldOrder：同一条带 trace_id/req_id 的记录，
// console(NoColor) 与 fileText 的字段出现先后一致，锁死「共享顺序逻辑」契约。
// 顺序：trace_id → req_id → 消息 → 其余 attrs(user)。
func TestConsoleAndFileTextShareFieldOrder(t *testing.T) {
	ctx := WithReqID(WithTraceID(context.Background(), "t-1"), "r-1")

	var cbuf, fbuf bytes.Buffer
	console := NewTraceHandler(NewConsoleHandler(&cbuf, &ConsoleOptions{Level: slog.LevelInfo, NoColor: true}))
	fileText := NewTraceHandler(newFileTextHandler(&fbuf, &FileTextOptions{Level: slog.LevelInfo}))

	render := func(h slog.Handler, out *bytes.Buffer) string {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "shared order", 0)
		r.AddAttrs(slog.String("user", "bob"))
		if err := h.Handle(ctx, r); err != nil {
			t.Fatalf("handle: %v", err)
		}
		return strings.TrimSpace(out.String())
	}

	cline := render(console, &cbuf)
	fline := render(fileText, &fbuf)

	keys := []string{"trace_id=", "req_id=", "shared order", "user="}
	assertFieldOrder(t, cline, keys)
	assertFieldOrder(t, fline, keys)
}

// TestScanTraceIDsIgnoresNonStringAndGroup：挑选判据边界——
// Group 内的同名 key、以及非 String 的顶层同名 key，均不提取（保持普通属性输出）。
func TestScanTraceIDsIgnoresNonStringAndGroup(t *testing.T) {
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "m", 0)
	r.AddAttrs(
		slog.Group("meta", slog.String(AttrTraceID, "in-group")), // Group 内：忽略
		slog.Int(AttrReqID, 42),                                  // 顶层但非 String：忽略
		slog.String("k", "v"),
	)
	traceID, reqID := scanTraceIDs(r)
	if traceID != "" || reqID != "" {
		t.Errorf("expected no ids extracted from group/non-string, got trace=%q req=%q", traceID, reqID)
	}
	// 去重判据此时不应跳过这些 attr（它们应作为普通属性输出）
	r.Attrs(func(a slog.Attr) bool {
		if isTraceIDAttr(a, traceID, reqID) {
			t.Errorf("attr %q should not be deduped", a.Key)
		}
		return true
	})
}
