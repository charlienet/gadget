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

// textformat_parity_internal_test.go —— 文件 text sink（FormatText）即 console 渲染器
// NoColor 形态的单一实现契约测试。
//
// 分两部分：
//  1. 等同矩阵 TestConsoleNoColorByteIdenticalToFileText：同一 record 分别喂
//     NewConsoleHandler(NoColor:true) 与 newFileHandler(w, FormatText, handlerOpts)，
//     断言整行字节等同（评审 ISSUE-002；用例②兼锁 ISSUE-001 零时间不留行首空格；
//     用例⑦兼锁 console attr quoting 行为，评审 ISSUE-003 的判别力锚点）。
//  2. 原 text_file_internal_test.go 的有判别力 golden 用例迁移：对合并后 handler 的
//     NoColor 形态断言，source= 按新契约位于行尾（其余 attrs 之后、'\n' 之前）。

// --- ① 双通道字节等同矩阵 ---

// newFileTextSink 构造文件 text sink handler（FormatText 分支，经 newFileHandler 走真实装配路径）。
func newFileTextSink(buf *bytes.Buffer, addSource bool) slog.Handler {
	return newFileHandler(buf, FormatText, &slog.HandlerOptions{
		Level:     slog.LevelInfo,
		AddSource: addSource,
	})
}

func TestConsoleNoColorByteIdenticalToFileText(t *testing.T) {
	fixed := time.Date(2026, 9, 24, 10, 12, 33, 456_000_000, time.UTC)
	const ts = "2026-09-24 10:12:33.456"

	cases := []struct {
		name   string
		build  func() slog.Record
		derive func(slog.Handler) slog.Handler
		want   string // NoColor 形态整行锚定（防两通道同错）；为空则跳过整行锚定（source 值动态，见⑧）
		// addSource 为 true 时两 handler 均开 AddSource（用例⑧：source 值含
		// 动态 file:line，无法静态锚定整行，改由 extra 做结构性断言）
		addSource bool
		extra     func(t *testing.T, got string)
	}{
		{
			// ① 正常：time + service/env 前置 + msg + attrs
			name: "normal",
			derive: func(h slog.Handler) slog.Handler {
				return h.WithAttrs([]slog.Attr{
					slog.String(AttrService, "opencode-api"),
					slog.String(AttrEnv, "prod"),
				})
			},
			build: func() slog.Record {
				r := slog.NewRecord(fixed, slog.LevelInfo, "fetching user", 0)
				r.AddAttrs(slog.Int("user_id", 42))
				return r
			},
			want: ts + " INFO opencode-api prod fetching user user_id=42\n",
		},
		{
			// ② time 零值：行首不得有空格（ISSUE-001 判别用例）
			name: "zero-time",
			build: func() slog.Record {
				return slog.NewRecord(time.Time{}, slog.LevelInfo, "hello", 0)
			},
			want: "INFO hello\n",
		},
		{
			// ③ WithGroup("g")：attr key 带点分前缀
			name:   "with-group",
			derive: func(h slog.Handler) slog.Handler { return h.WithGroup("g") },
			build: func() slog.Record {
				r := slog.NewRecord(fixed, slog.LevelInfo, "m", 0)
				r.AddAttrs(slog.String("k", "v"))
				return r
			},
			want: ts + " INFO m g.k=v\n",
		},
		{
			// ④ record 内 slog.Group attr：含带空格 string + int，递归展开带前缀
			name: "group-attr",
			build: func() slog.Record {
				r := slog.NewRecord(fixed, slog.LevelInfo, "m", 0)
				r.AddAttrs(slog.Group("db", slog.String("host", "local host"), slog.Int("port", 5432)))
				return r
			},
			want: ts + ` INFO m db.host="local host" db.port=5432` + "\n",
		},
		{
			// ⑤ 前置字段值含空格：触发 appendQuoting，裸值段加引号
			name: "front-value-with-space",
			derive: func(h slog.Handler) slog.Handler {
				return h.WithAttrs([]slog.Attr{slog.String(AttrService, "svc b")})
			},
			build: func() slog.Record {
				return slog.NewRecord(fixed, slog.LevelInfo, "m", 0)
			},
			want: ts + ` INFO "svc b" m` + "\n",
		},
		{
			// ⑥ 空 message：msg 段仍按 sep 补空格（现状语义，双通道一致）
			name: "empty-message",
			build: func() slog.Record {
				return slog.NewRecord(fixed, slog.LevelInfo, "", 0)
			},
			want: ts + " INFO \n",
		},
		{
			// ⑦ attr string 含空格：quoting 行为锚定（ISSUE-003 判别用例，
			// 反向验证时把 appendTextValue 改回 appendValue 本用例必红）
			name: "attr-string-with-space",
			build: func() slog.Record {
				r := slog.NewRecord(fixed, slog.LevelInfo, "m", 0)
				r.AddAttrs(slog.String("path", "a b"), slog.Int("n", 5))
				return r
			},
			want: ts + ` INFO m path="a b" n=5` + "\n",
		},
		{
			// ⑧ add-source：双通道均 AddSource=true；msg 含换行——把 msg 转义
			// 行为锁进等同矩阵（字面 \n、单行），并锚定 source= 位于行尾
			name:      "add-source",
			addSource: true,
			derive: func(h slog.Handler) slog.Handler {
				return h.WithAttrs([]slog.Attr{
					slog.String(AttrService, "svc"),
					slog.String(AttrEnv, "prod"),
				})
			},
			build: func() slog.Record {
				pc, _, _, _ := runtime.Caller(0) // 真实 PC，source 可解析
				r := slog.NewRecord(fixed, slog.LevelInfo, "src msg\nwith newline", pc)
				r.AddAttrs(slog.Int("n", 1), slog.String("k", "v"))
				return r
			},
			extra: func(t *testing.T, got string) {
				line := strings.TrimSpace(got)
				// msg 段转义为字面反斜杠n，整行仅行尾一个真实换行
				if !strings.Contains(line, `src msg\nwith newline`) {
					t.Errorf("expected msg escaped to literal backslash-n, got: %q", line)
				}
				if strings.Count(got, "\n") != 1 {
					t.Errorf("expected single line, got %d newlines: %q", strings.Count(got, "\n"), got)
				}
				// source= 存在且位于行尾（最后一个字段）
				idx := strings.Index(line, "source=")
				if idx < 0 {
					t.Fatalf("expected source= with AddSource, got: %q", line)
				}
				if strings.Contains(line[idx+len("source="):], " ") {
					t.Errorf("expected source= as last field at line end, got: %q", line[idx:])
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cbuf, fbuf bytes.Buffer
			ch := slog.Handler(NewConsoleHandler(&cbuf, &ConsoleOptions{
				Level: slog.LevelInfo, NoColor: true, AddSource: tc.addSource,
			}))
			fh := newFileTextSink(&fbuf, tc.addSource)
			if tc.derive != nil {
				ch = tc.derive(ch)
				fh = tc.derive(fh)
			}

			r := tc.build()
			if err := ch.Handle(context.Background(), r); err != nil {
				t.Fatalf("console handle: %v", err)
			}
			if err := fh.Handle(context.Background(), r); err != nil {
				t.Fatalf("file text handle: %v", err)
			}

			gotC, gotF := cbuf.String(), fbuf.String()
			if gotC != gotF {
				t.Errorf("console(NoColor) vs file Text not byte-identical:\n console: %q\n file:    %q", gotC, gotF)
			}
			if tc.want != "" && gotC != tc.want {
				t.Errorf("golden mismatch:\n got:  %q\nwant: %q", gotC, tc.want)
			}
			if tc.extra != nil {
				tc.extra(t, gotC)
			}
			t.Logf("line: %q", gotC)
		})
	}
}

// --- ② golden 用例迁移（原 text_file_internal_test.go，断言合并后 NoColor 形态） ---

// TestFileTextHandlerFieldOrder：注入含 trace_id/req_id 的 record（经 TraceHandler 包一层），
// 断言两字段以裸值（无 key= 前缀）出现在 msg 原样文本之前，且 msg 之后不再重复出现。
func TestFileTextHandlerFieldOrder(t *testing.T) {
	var buf bytes.Buffer
	th := NewTraceHandler(newFileTextSink(&buf, false))

	ctx := WithReqID(WithTraceID(context.Background(), "trace-abc"), "req-xyz")
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello world", 0)
	r.AddAttrs(slog.String("user", "bob"))
	if err := th.Handle(ctx, r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	msgIdx := strings.Index(line, "hello world")
	traceIdx := strings.Index(line, "trace-abc")
	reqIdx := strings.Index(line, "req-xyz")
	if msgIdx < 0 || traceIdx < 0 || reqIdx < 0 {
		t.Fatalf("expected bare trace/req values and raw msg present, got: %s", line)
	}
	if traceIdx >= msgIdx || reqIdx >= msgIdx {
		t.Errorf("expected trace/req bare values before msg, got: %s", line)
	}
	// trace 值在 req 值之前，user=bob（k=v 不变）在 msg 之后
	if traceIdx >= reqIdx {
		t.Errorf("expected trace_id before req_id, got: %s", line)
	}
	userIdx := strings.Index(line, "user=")
	if userIdx <= msgIdx {
		t.Errorf("expected regular attr after msg, got: %s", line)
	}
	// 前置段与 msg 均无 key= 前缀
	if strings.Contains(line, "msg=") || strings.Contains(line, "trace_id=") || strings.Contains(line, "req_id=") {
		t.Errorf("front segment and msg must be bare values without key=, got: %s", line)
	}
	// 去重：msg 之后不得再出现 trace/req 的值或 key
	afterMsg := line[msgIdx:]
	if strings.Contains(afterMsg, "trace") || strings.Contains(afterMsg, "req") {
		t.Errorf("trace/req duplicated after msg: %s", afterMsg)
	}
}

// TestFileTextHandlerNoTrace：ctx 未注入 trace/req 时，不输出这两个 key。
func TestFileTextHandlerNoTrace(t *testing.T) {
	var buf bytes.Buffer
	h := newFileTextSink(&buf, false)

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "plain", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	line := buf.String()
	if strings.Contains(line, "trace_id") || strings.Contains(line, "req_id") {
		t.Errorf("expected no trace/req keys, got: %s", line)
	}
}

// TestFileTextHandlerMessageRaw：msg 原样输出——不加引号（含空格也不加）、无 key= 前缀。
func TestFileTextHandlerMessageRaw(t *testing.T) {
	var buf bytes.Buffer
	h := newFileTextSink(&buf, false)

	r1 := slog.NewRecord(time.Now(), slog.LevelInfo, "含 空格", 0)
	if err := h.Handle(context.Background(), r1); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); !strings.HasSuffix(got, "含 空格") || strings.Contains(got, `"含 空格"`) {
		t.Errorf("expected raw msg with space (no quotes, no key), got: %s", got)
	}

	buf.Reset()
	r2 := slog.NewRecord(time.Now(), slog.LevelInfo, "word", 0)
	if err := h.Handle(context.Background(), r2); err != nil {
		t.Fatalf("handle: %v", err)
	}
	line2 := strings.TrimSpace(buf.String())
	if !strings.HasSuffix(line2, " word") || strings.Contains(line2, "msg=") {
		t.Errorf("expected bare msg word at line end without msg=, got: %s", line2)
	}
}

// TestFileTextHandlerAttrValueQuoting：字符串属性值含空格时同样加引号；非字符串值原样。
func TestFileTextHandlerAttrValueQuoting(t *testing.T) {
	var buf bytes.Buffer
	h := newFileTextSink(&buf, false)

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

// TestFileTextHandlerTimeFormat：时间戳为裸值、格式固定 "2006-01-02 15:04:05.000"，
// 位于行首（无 time= 前缀），后接裸级别（无 level= 前缀）。
func TestFileTextHandlerTimeFormat(t *testing.T) {
	var buf bytes.Buffer
	h := newFileTextSink(&buf, false)

	ts := time.Date(2026, 9, 8, 13, 5, 7, 123_000_000, time.UTC)
	r := slog.NewRecord(ts, slog.LevelInfo, "msg", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	re := regexp.MustCompile(`^2026-09-08 13:05:07\.123 INFO msg$`)
	if !re.MatchString(line) {
		t.Errorf("expected bare time/level/msg at line start, got: %s", line)
	}
}

// TestFileTextHandlerAddSource：AddSource=true 时含 source=（k=v 风格不变），
// 位于 msg 与其余 attrs 之后、行尾（新契约：source 排最后）。
func TestFileTextHandlerAddSource(t *testing.T) {
	var buf bytes.Buffer
	h := newFileTextSink(&buf, true)

	pc, _, _, ok := runtime.Caller(0)
	if !ok {
		pc = 0
	}
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "src msg", pc)
	r.AddAttrs(slog.Int("n", 1))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	msgIdx := strings.Index(line, "src msg")
	attrIdx := strings.Index(line, "n=1")
	srcIdx := strings.Index(line, "source=")
	if srcIdx < 0 {
		t.Fatalf("expected source= when AddSource, got: %s", line)
	}
	if msgIdx < 0 || attrIdx < 0 || attrIdx <= msgIdx {
		t.Fatalf("expected msg then attrs, got: %s", line)
	}
	// source= 位于其余 attrs 之后（行尾最后一个字段）
	if srcIdx <= attrIdx {
		t.Errorf("expected source= after regular attrs (line end), got: %s", line)
	}
	if !strings.HasPrefix(line[srcIdx:], "source=") {
		t.Errorf("expected source= as last field at line end, got: %s", line[srcIdx:])
	}
	if strings.Contains(line, "msg=") {
		t.Errorf("msg must be raw without msg= prefix, got: %s", line)
	}
}

// TestFileTextHandlerWithAttrsBeforeRecord：WithAttrs 累积属性排在 record 自身属性之前。
func TestFileTextHandlerWithAttrsBeforeRecord(t *testing.T) {
	var buf bytes.Buffer
	h := newFileTextSink(&buf, false).WithAttrs([]slog.Attr{slog.String("preset", "p1")})

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
	h := newFileTextSink(&buf, false).WithGroup("cfg")

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

// TestFileTextHandlerFrontBareValues：前置段（time/level/service/env）与 msg 全部裸值、
// 无 key= 前缀；零配置时 service/env 整段消失；source= 与其余 attrs 保持 k=v。
func TestFileTextHandlerFrontBareValues(t *testing.T) {
	ts := time.Date(2026, 9, 24, 10, 12, 33, 456_000_000, time.UTC)

	// 零配置：无 service/env/trace/req → 前置字段整段消失，仅剩 time level msg attrs
	var buf bytes.Buffer
	h := newFileTextSink(&buf, false)
	r := slog.NewRecord(ts, slog.LevelInfo, "fetching user", 0)
	r.AddAttrs(slog.Int("user_id", 42))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got, want := strings.TrimSpace(buf.String()), "2026-09-24 10:12:33.456 INFO fetching user user_id=42"; got != want {
		t.Errorf("zero-config golden mismatch:\n got: %s\nwant: %s", got, want)
	}

	// service/env 有值：裸值前置（无 service= / env= 前缀），msg 含空格原样，attrs 仍 k=v
	buf.Reset()
	h2 := newFileTextSink(&buf, false).WithAttrs([]slog.Attr{
		slog.String("service", "opencode-api"),
		slog.String("env", "prod"),
	})
	r2 := slog.NewRecord(ts, slog.LevelWarn, "slow query", 0)
	r2.AddAttrs(slog.String("db", "users"), slog.Int("elapsed_ms", 1234))
	if err := h2.Handle(context.Background(), r2); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got, want := strings.TrimSpace(buf.String()), "2026-09-24 10:12:33.456 WARN opencode-api prod slow query db=users elapsed_ms=1234"; got != want {
		t.Errorf("front bare values golden mismatch:\n got: %s\nwant: %s", got, want)
	}
}
