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

// TestConsoleAndFileTextShareFieldOrder：同一条带 service/env（With 预设）+ trace_id/req_id
// （ctx 注入）的记录，console(NoColor) 与 fileText 的四前置字段出现先后一致，
// 锁死「共享顺序逻辑」契约。顺序：service → env → trace_id → req_id → 消息 → 其余 attrs(user)。
func TestConsoleAndFileTextShareFieldOrder(t *testing.T) {
	ctx := WithReqID(WithTraceID(context.Background(), "t-1"), "r-1")

	var cbuf, fbuf bytes.Buffer
	preset := []slog.Attr{slog.String(AttrService, "svc-1"), slog.String(AttrEnv, "prod")}
	console := NewTraceHandler(NewConsoleHandler(&cbuf, &ConsoleOptions{Level: slog.LevelInfo, NoColor: true})).WithAttrs(preset)
	fileText := NewTraceHandler(newFileTextHandler(&fbuf, &FileTextOptions{Level: slog.LevelInfo})).WithAttrs(preset)

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

	keys := []string{"service=", "env=", "trace_id=", "req_id=", "shared order", "user="}
	assertFieldOrder(t, cline, keys)
	assertFieldOrder(t, fline, keys)
}

// TestScanFrontFieldsIgnoresNonStringAndGroup：挑选判据边界——
// Group 内的同名 key、非 String 的顶层同名 key、以及值为空串的顶层 key，均不触发前置（保持普通属性输出）。
func TestScanFrontFieldsIgnoresNonStringAndGroup(t *testing.T) {
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "m", 0)
	r.AddAttrs(
		slog.Group("meta", slog.String(AttrService, "in-group")), // Group 内：忽略
		slog.Int(AttrEnv, 42),        // 顶层但非 String：忽略
		slog.String(AttrTraceID, ""), // 顶层 String 但空串：视为未命中
		slog.String("k", "v"),
	)
	picked := scanRecordFrontFields(r)
	for _, key := range frontFieldKeys {
		if picked.get(key) != "" {
			t.Errorf("expected no value for %q (group/non-string/empty), got %q", key, picked.get(key))
		}
	}
	// 去重判据此时不应跳过这些 attr（它们应作为普通属性输出）
	r.Attrs(func(a slog.Attr) bool {
		if isPickedFrontAttr(a, picked, false) {
			t.Errorf("attr %q should not be deduped", a.Key)
		}
		return true
	})
}

// --- 双源挑选/去重契约：console 与 fileText 各跑一遍 ---

// traceRenderCase 抽象两个自研 handler 的构造差异，便于同一契约测试各跑一遍。
type traceRenderCase struct {
	name  string
	build func(*bytes.Buffer) slog.Handler
}

func traceRenderCases() []traceRenderCase {
	return []traceRenderCase{
		{
			name: "console",
			build: func(b *bytes.Buffer) slog.Handler {
				return NewConsoleHandler(b, &ConsoleOptions{Level: slog.LevelInfo, NoColor: true})
			},
		},
		{
			name: "fileText",
			build: func(b *bytes.Buffer) slog.Handler {
				return newFileTextHandler(b, &FileTextOptions{Level: slog.LevelInfo})
			},
		},
	}
}

// renderTraceLine 渲染单条记录并返回 trim 后的行文本。
func renderTraceLine(t *testing.T, h slog.Handler, buf *bytes.Buffer, ctx context.Context, msg string) string {
	t.Helper()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0)
	if err := h.Handle(ctx, r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	return strings.TrimSpace(buf.String())
}

// assertPromotedBeforeMsg 断言 key=value 出现在 msg 之前，且该 key 全行仅出现一次。
func assertPromotedBeforeMsg(t *testing.T, line, msg, keyValue string) {
	t.Helper()
	msgIdx := strings.Index(line, msg)
	idx := strings.Index(line, keyValue)
	if msgIdx < 0 || idx < 0 {
		t.Fatalf("expected %q and msg %q present in: %q", keyValue, msg, line)
	}
	if idx >= msgIdx {
		t.Errorf("expected %q before msg, got: %q", keyValue, line)
	}
	key := keyValue[:strings.Index(keyValue, "=")+1]
	if n := strings.Count(line, key); n != 1 {
		t.Errorf("expected key %q exactly once, got %d in: %q", key, n, line)
	}
}

// TestWithPresetTraceIDsPromoted：With 预设的原生 string trace_id/req_id 同样前置到 msg 之前，
// 且各只输出一次（不再作为普通属性落在尾部）。
func TestWithPresetTraceIDsPromoted(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := tc.build(&buf).WithAttrs([]slog.Attr{
				slog.String(AttrTraceID, "t-with"),
				slog.String(AttrReqID, "r-with"),
			})
			line := renderTraceLine(t, h, &buf, context.Background(), "hello")

			assertPromotedBeforeMsg(t, line, "hello", "trace_id=t-with")
			assertPromotedBeforeMsg(t, line, "hello", "req_id=r-with")
			// 锁两 key 相对次序：trace_id → req_id → msg
			assertFieldOrder(t, line, []string{"trace_id=", "req_id=", "hello"})
		})
	}
}

// TestRecordWinsOverWithTraceIDs：record（ctx 注入）与 h.attrs（With）双源并存时，
// 前置输出 record 值，h.attrs 同名值被去重剔除，两 key 各只出现一次。
func TestRecordWinsOverWithTraceIDs(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			inner := tc.build(&buf).WithAttrs([]slog.Attr{
				slog.String(AttrTraceID, "t-with"),
				slog.String(AttrReqID, "r-with"),
			})
			h := NewTraceHandler(inner)
			ctx := WithReqID(WithTraceID(context.Background(), "t-ctx"), "r-ctx")

			line := renderTraceLine(t, h, &buf, ctx, "hello")

			assertPromotedBeforeMsg(t, line, "hello", "trace_id=t-ctx")
			assertPromotedBeforeMsg(t, line, "hello", "req_id=r-ctx")
			if strings.Contains(line, "t-with") || strings.Contains(line, "r-with") {
				t.Errorf("expected h.attrs values deduped, got: %q", line)
			}
		})
	}
}

// TestWithAttrsDuplicateTraceIDTakesLast：h.attrs 内同一 key 多次 With 时取最后一次出现的值，
// 其余全部剔除，全行只输出一次。
func TestWithAttrsDuplicateTraceIDTakesLast(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := tc.build(&buf).
				WithAttrs([]slog.Attr{slog.String(AttrTraceID, "t-first")}).
				WithAttrs([]slog.Attr{slog.String(AttrTraceID, "t-second")})

			line := renderTraceLine(t, h, &buf, context.Background(), "hello")

			assertPromotedBeforeMsg(t, line, "hello", "trace_id=t-second")
			if strings.Contains(line, "t-first") {
				t.Errorf("expected earlier duplicate dropped, got: %q", line)
			}
		})
	}
}

// TestWithGroupTraceIDNotPromoted：WithGroup 后 With 的 trace_id/req_id 带前缀，不参与前置挑选，
// 作为普通属性出现在 msg 之后。
func TestWithGroupTraceIDNotPromoted(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := tc.build(&buf).WithGroup("g").WithAttrs([]slog.Attr{slog.String(AttrReqID, "r-group")})

			line := renderTraceLine(t, h, &buf, context.Background(), "hello")

			msgIdx := strings.Index(line, "hello")
			if msgIdx < 0 {
				t.Fatalf("expected msg in: %q", line)
			}
			if strings.Contains(line[:msgIdx], "req_id") {
				t.Errorf("expected no promoted req_id before msg, got: %q", line)
			}
			if !strings.Contains(line[msgIdx:], "g.req_id=r-group") {
				t.Errorf("expected prefixed req_id as regular attr after msg, got: %q", line)
			}
		})
	}
}

// TestNonStringTraceIDNotPromoted：非 String kind（slog.Int）的同名 key 不命中，
// 作为普通属性出现在 msg 之后。
func TestNonStringTraceIDNotPromoted(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := tc.build(&buf).WithAttrs([]slog.Attr{slog.Int(AttrReqID, 42)})

			line := renderTraceLine(t, h, &buf, context.Background(), "hello")

			msgIdx := strings.Index(line, "hello")
			if msgIdx < 0 {
				t.Fatalf("expected msg in: %q", line)
			}
			if strings.Contains(line[:msgIdx], "req_id") {
				t.Errorf("expected no promoted req_id before msg, got: %q", line)
			}
			if !strings.Contains(line[msgIdx:], "req_id=42") {
				t.Errorf("expected non-string req_id as regular attr after msg, got: %q", line)
			}
		})
	}
}

// --- service/env 纳入前置布局：与 trace_id/req_id 同判据（console/fileText 双路各跑） ---

// TestWithPresetAllFrontFieldsPromoted：With 预设 service/env/trace_id/req_id（原生 string，
// 模拟 New() preset 路径）→ 全部前置到 msg 之前，且四 key 相对次序锁死。
func TestWithPresetAllFrontFieldsPromoted(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := tc.build(&buf).WithAttrs([]slog.Attr{
				slog.String(AttrService, "svc"),
				slog.String(AttrEnv, "prod"),
				slog.String(AttrTraceID, "t"),
				slog.String(AttrReqID, "r"),
			})
			line := renderTraceLine(t, h, &buf, context.Background(), "hello")

			// 锁死四 key 相对次序：service → env → trace_id → req_id → msg
			assertFieldOrder(t, line, []string{"service=", "env=", "trace_id=", "req_id=", "hello"})
			for _, kv := range []string{"service=svc", "env=prod", "trace_id=t", "req_id=r"} {
				assertPromotedBeforeMsg(t, line, "hello", kv)
			}
		})
	}
}

// TestRecordWinsOverWithService：record（业务显式 attr）与 h.attrs（With 预设）双源并存时，
// 前置输出 record 值，With 同名值被去重剔除，全行只出现一次。
func TestRecordWinsOverWithService(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := tc.build(&buf).WithAttrs([]slog.Attr{slog.String(AttrService, "svc-with")})
			r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
			r.AddAttrs(slog.String(AttrService, "svc-rec")) // record 级注入
			if err := h.Handle(context.Background(), r); err != nil {
				t.Fatalf("handle: %v", err)
			}
			line := strings.TrimSpace(buf.String())

			assertPromotedBeforeMsg(t, line, "hello", "service=svc-rec")
			if strings.Contains(line, "svc-with") {
				t.Errorf("expected With service deduped, got: %q", line)
			}
		})
	}
}

// TestWithAttrsDuplicateServiceTakesLast：h.attrs 内同一 service 多次 With → 取最后一次且单一输出。
func TestWithAttrsDuplicateServiceTakesLast(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := tc.build(&buf).
				WithAttrs([]slog.Attr{slog.String(AttrService, "svc-first")}).
				WithAttrs([]slog.Attr{slog.String(AttrService, "svc-second")})

			line := renderTraceLine(t, h, &buf, context.Background(), "hello")

			assertPromotedBeforeMsg(t, line, "hello", "service=svc-second")
			if strings.Contains(line, "svc-first") {
				t.Errorf("expected earlier duplicate dropped, got: %q", line)
			}
		})
	}
}

// TestWithGroupServiceNotPromoted：WithGroup 后 With 的 service 带前缀，不参与前置挑选，
// 作为普通属性出现在 msg 之后。
func TestWithGroupServiceNotPromoted(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := tc.build(&buf).WithGroup("g").WithAttrs([]slog.Attr{slog.String(AttrService, "svc-group")})

			line := renderTraceLine(t, h, &buf, context.Background(), "hello")

			msgIdx := strings.Index(line, "hello")
			if msgIdx < 0 {
				t.Fatalf("expected msg in: %q", line)
			}
			if strings.Contains(line[:msgIdx], "service") {
				t.Errorf("expected no promoted service before msg, got: %q", line)
			}
			if !strings.Contains(line[msgIdx:], "g.service=svc-group") {
				t.Errorf("expected prefixed service as regular attr after msg, got: %q", line)
			}
		})
	}
}

// TestNonStringEnvNotPromoted：非 String kind（slog.Int）的 env 不命中，作为普通属性出现在 msg 之后。
func TestNonStringEnvNotPromoted(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := tc.build(&buf).WithAttrs([]slog.Attr{slog.Int(AttrEnv, 42)})

			line := renderTraceLine(t, h, &buf, context.Background(), "hello")

			msgIdx := strings.Index(line, "hello")
			if msgIdx < 0 {
				t.Fatalf("expected msg in: %q", line)
			}
			if strings.Contains(line[:msgIdx], "env=") {
				t.Errorf("expected no promoted env before msg, got: %q", line)
			}
			if !strings.Contains(line[msgIdx:], "env=42") {
				t.Errorf("expected non-string env as regular attr after msg, got: %q", line)
			}
		})
	}
}

// TestEmptyRecordServiceYieldsWith：空串语义——record 存在 service 但值为空串时视为未命中，
// 不阻断 h.attrs（With）值顶位前置（与 TraceHandler ctx 空值过滤一致）。
func TestEmptyRecordServiceYieldsWith(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := tc.build(&buf).WithAttrs([]slog.Attr{slog.String(AttrService, "svc-with")})
			r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
			r.AddAttrs(slog.String(AttrService, "")) // record 有 service 但值为空串
			if err := h.Handle(context.Background(), r); err != nil {
				t.Fatalf("handle: %v", err)
			}
			line := strings.TrimSpace(buf.String())

			// With 值顶位前置，且全行 service 只出现一次（空串 record 项亦被去重）
			assertPromotedBeforeMsg(t, line, "hello", "service=svc-with")
		})
	}
}

// TestMixedSourceFrontFields：交叉来源显式用例——h.attrs（With）预设 service，record 顶层注入
// env（非空 string）。期望前置次序 service（来自 attrs）→ env（来自 record），两值正确、
// 各唯一、均在 msg 之前。
func TestMixedSourceFrontFields(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := tc.build(&buf).WithAttrs([]slog.Attr{slog.String(AttrService, "svc-with")})
			r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
			r.AddAttrs(slog.String(AttrEnv, "env-rec")) // record 级注入 env
			if err := h.Handle(context.Background(), r); err != nil {
				t.Fatalf("handle: %v", err)
			}
			line := strings.TrimSpace(buf.String())

			// 两值均前置、各唯一
			assertPromotedBeforeMsg(t, line, "hello", "service=svc-with")
			assertPromotedBeforeMsg(t, line, "hello", "env=env-rec")
			// 交叉来源下仍按 frontFieldKeys 固定次序：service（attrs）→ env（record）→ msg
			assertFieldOrder(t, line, []string{"service=", "env=", "hello"})
		})
	}
}

// TestMixedSourceServiceAttrsTraceRecord：X1 实证组合——record（经 TraceHandler 从 ctx 注入）
// 携带 trace_id，h.attrs（With）预设 service。期望前置次序 service → trace_id → msg，
// 锁不同 key 分属两源时的相对次序与去重。
func TestMixedSourceServiceAttrsTraceRecord(t *testing.T) {
	for _, tc := range traceRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			inner := tc.build(&buf).WithAttrs([]slog.Attr{slog.String(AttrService, "svc-with")})
			h := NewTraceHandler(inner)
			ctx := WithTraceID(context.Background(), "t-ctx")

			line := renderTraceLine(t, h, &buf, ctx, "hello")

			assertPromotedBeforeMsg(t, line, "hello", "service=svc-with")
			assertPromotedBeforeMsg(t, line, "hello", "trace_id=t-ctx")
			// service（attrs 源）先于 trace_id（record 源），二者均前置
			assertFieldOrder(t, line, []string{"service=", "trace_id=", "hello"})
		})
	}
}
