package logger

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 本地 mock 收集端（模拟对端 Vector 0.58 sources.http_server）----

// httpMockReq 一次请求的落地快照。只存标量与已解析行（不存 http.Header map），
// 避免 mock goroutine 与测试 goroutine 并发读写 map 触发 -race。
type httpMockReq struct {
	contentType     string
	contentEncoding string
	auth            string
	extra           string
	gzipped         bool // 请求头含 Content-Encoding: gzip
	code            int  // 本端应答码（200 / 500）
	body            string
	lines           []string
}

// httpMock 累积式收集端：每个 POST 请求按 gzip 解压 → NDJSON 逐行解析后入快照队列。
// failures = 前 N 个请求直接返 500（不解析，模拟服务端故障）；delay 模拟慢端点。
type httpMock struct {
	t     *testing.T
	srv   *httptest.Server
	path  string
	url   string // 完整端点（含 path），即 handler 的投递目标
	calls int32

	mu   sync.Mutex
	reqs []httpMockReq
}

func newHTTPMock(t *testing.T) *httpMock {
	t.Helper()
	return newHTTPMockOpts(t, httpMockOpts{})
}

type httpMockOpts struct {
	failures int           // 前 failures 个请求返 500
	delay    time.Duration // 每个请求处理前 sleep（模拟慢端点）
	code     int           // 非 0 → 所有请求一律应答该状态码（覆盖 failures）
}

func newHTTPMockOpts(t *testing.T, o httpMockOpts) *httpMock {
	t.Helper()
	m := &httpMock{t: t, path: "/v1/logs"}
	mux := http.NewServeMux()
	mux.HandleFunc(m.path, func(w http.ResponseWriter, r *http.Request) {
		if o.delay > 0 {
			time.Sleep(o.delay)
		}
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(r.Body)
		_ = r.Body.Close()

		m.mu.Lock()
		m.calls++
		call := m.calls
		m.mu.Unlock()

		// 无论应答成功或失败都留快照：重试语义（同一批被尝试 N 次）必须可断言。
		gz := r.Header.Get("Content-Encoding") == "gzip"
		body := raw.Bytes()
		if gz {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Errorf("mock: gzip reader: %v", err)
			} else {
				var out bytes.Buffer
				if _, err := out.ReadFrom(zr); err != nil {
					t.Errorf("mock: gunzip: %v", err)
				}
				_ = zr.Close()
				body = out.Bytes()
			}
		}
		var lines []string
		if len(body) > 0 {
			lines = strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
		}
		code := http.StatusOK
		switch {
		case o.code != 0:
			code = o.code // 固定应答码（4xx 不重试语义用例）
		case int(call) <= o.failures:
			code = http.StatusInternalServerError
		}
		snap := httpMockReq{
			contentType:     r.Header.Get("Content-Type"),
			contentEncoding: r.Header.Get("Content-Encoding"),
			auth:            r.Header.Get("Authorization"),
			extra:           r.Header.Get("X-Extra"),
			gzipped:         gz,
			code:            code,
			body:            string(body),
			lines:           lines,
		}

		m.mu.Lock()
		m.reqs = append(m.reqs, snap)
		m.mu.Unlock()

		if code != http.StatusOK {
			w.WriteHeader(code)
			_, _ = w.Write([]byte("mock boom"))
			return
		}
		w.WriteHeader(code)
	})
	m.srv = httptest.NewServer(mux)
	m.url = m.srv.URL + m.path
	t.Cleanup(m.srv.Close)
	return m
}

// waitReqs 轮询等待至少 n 个请求落定，返回快照；超时 t.Fatal。
func (m *httpMock) waitReqs(n int, timeout time.Duration) []httpMockReq {
	m.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		m.mu.Lock()
		got := append([]httpMockReq(nil), m.reqs...)
		m.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			m.t.Fatalf("等待 %d 个请求超时（实收 %d）", n, len(got))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// reqCount 当前已收到的请求数。
func (m *httpMock) reqCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reqs)
}

// waitIdle 等待「连续 stable 时间内没有新请求」（判定 inflight 重试链已全部落定）。
func (m *httpMock) waitIdle(stable, timeout time.Duration) int {
	m.t.Helper()
	deadline := time.Now().Add(timeout)
	last, lastChange := -1, time.Now()
	for {
		n := m.reqCount()
		if n != last {
			last, lastChange = n, time.Now()
		} else if n > 0 && time.Since(lastChange) >= stable {
			return n
		}
		if time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---- handler 构建辅助 ----

// newTestHTTPHandler 直接构建 http handler（绕过装配层），URL 指向 mock。
// 默认 BatchSize=1、FlushInterval=10s（只测批量边界时用大 BatchSize 覆盖）、
// Timeout=2s、retryBase=10ms（缩短重试节奏，字段写入在建 handler 后、任何日志写入前，
// 此刻 worker 必然空闲，无并发访问）。
func newTestHTTPHandler(t *testing.T, url string, batch int) (*httpHandler, *httpSink) {
	t.Helper()
	if batch <= 0 {
		batch = 1
	}
	h, s := newHTTPHandler(&HTTPSettings{
		URL:           url,
		BatchSize:     batch,
		FlushInterval: 10 * time.Second,
		Timeout:       2 * time.Second,
	}, "", slog.LevelDebug, false)
	s.retryBase = 10 * time.Millisecond
	s.stderr = &stderrBuf{}
	t.Cleanup(func() { _ = s.Close() })
	return h, s
}

// stderrBuf 并发安全的告警缓冲（告警由发送 worker 写、测试 goroutine 读）。
type stderrBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *stderrBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *stderrBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitDropped 轮询等待 dropped 计数达到 n（丢弃由 worker 异步累计）。
func waitDropped(t *testing.T, s *httpSink, n uint64, timeout time.Duration) uint64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		d := s.droppedCount()
		if d >= n {
			return d
		}
		if time.Now().After(deadline) {
			return d
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// httpSrcWhitelistRE 帧 src 对端落盘文件名白名单（[a-zA-Z0-9._-] 非空全匹配）。
var httpSrcWhitelistRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// parseNDJSONLine 断言单行是合法 JSON 并返回字段 map。
func parseNDJSONLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(line), &body); err != nil {
		t.Fatalf("NDJSON 行非法 JSON: %v (line=%q)", err, line)
	}
	return body
}

// parseHTTPFrame 按落盘字段契约解析一行 NDJSON 帧：有且仅有 src + message 两键（均为
// 非空字符串），返回两值。帧形偏离（多键 / 缺键 / 非字符串）即 t.Fatal。
func parseHTTPFrame(t *testing.T, line string) (src, message string) {
	t.Helper()
	body := parseNDJSONLine(t, line)
	if len(body) != 2 {
		t.Fatalf("帧应有且仅有 src+message 两键, got %v (line=%q)", body, line)
	}
	s, ok1 := body["src"].(string)
	m, ok2 := body["message"].(string)
	if !ok1 || s == "" {
		t.Fatalf("帧 src 缺失或为空: %v (line=%q)", body, line)
	}
	if !ok2 {
		t.Fatalf("帧 message 缺失或非字符串: %v (line=%q)", body, line)
	}
	return s, m
}

// textTimeLayoutLen / parseFrameTime 断言 message 头部为 text 版式的
// "2006-01-02 15:04:05.000" 时间裸值（与 console/文件 text 同源渲染）。
const textTimeLayout = "2006-01-02 15:04:05.000"

// assertFrameTimePrefix 断言 message 以定制时间格式开头，返回去掉时间前缀后的剩余文本。
func assertFrameTimePrefix(t *testing.T, message string) string {
	t.Helper()
	if len(message) <= len(textTimeLayout) {
		t.Fatalf("message 过短、不含时间前缀: %q", message)
	}
	if _, err := time.Parse(textTimeLayout, message[:len(textTimeLayout)]); err != nil {
		t.Fatalf("message 时间前缀 %q 不可按 %q 解析: %v", message[:len(textTimeLayout)], textTimeLayout, err)
	}
	return message[len(textTimeLayout)+1:] // 跳过时间 + 单个空格
}

// ---- 批量与触发 ----

// BatchSize 满批触发：3 条一请求，逐行可解析、字段/级别正确、帧序保持。
func TestHTTP_BatchSizeFlush(t *testing.T) {
	m := newHTTPMock(t)
	h, _ := newTestHTTPHandler(t, m.url, 3)

	for _, msg := range []string{"l1", "l2", "l3"} {
		if err := h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, msg, 0)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}

	reqs := m.waitReqs(1, 3*time.Second)
	if len(reqs) != 1 {
		t.Fatalf("满批应只发一个请求, got %d", len(reqs))
	}
	got := reqs[0]
	if len(got.lines) != 3 {
		t.Fatalf("一个请求应含 3 行 NDJSON, got %d: %q", len(got.lines), got.lines)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}
	for i, want := range []string{"l1", "l2", "l3"} {
		src, message := parseHTTPFrame(t, got.lines[i])
		if src != sanitizeHTTPSrc(host) {
			t.Errorf("第 %d 行 src=%q want %q（Src/Service 皆空回退 os.Hostname 经白名单化）", i+1, src, sanitizeHTTPSrc(host))
		}
		if !strings.Contains(message, want) {
			t.Errorf("第 %d 行 message=%q 缺正文 %s", i+1, message, want)
		}
		if !strings.Contains(message, "INFO") {
			t.Errorf("第 %d 行 message=%q 缺级别 INFO", i+1, message)
		}
	}

	// 第 4 条另起新批（未满批 → 不触发发送）
	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "l4", 0))
	time.Sleep(200 * time.Millisecond)
	if m.reqCount() != 1 {
		t.Errorf("未满批不应提前发送, got %d reqs", m.reqCount())
	}
}

// FlushInterval 到点把非空当前批换出发（不满批）。
func TestHTTP_IntervalFlush(t *testing.T) {
	m := newHTTPMock(t)
	h, s := newHTTPHandler(&HTTPSettings{
		URL:           m.url,
		BatchSize:     100, // 永不满批：只能靠 ticker 投递
		FlushInterval: 50 * time.Millisecond,
		Timeout:       2 * time.Second,
	}, "", slog.LevelDebug, false)
	s.retryBase = 10 * time.Millisecond
	s.stderr = &stderrBuf{}
	defer s.Close()

	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "tick", 0))
	reqs := m.waitReqs(1, 3*time.Second)
	if len(reqs[0].lines) != 1 {
		t.Fatalf("ticker 应送达 1 行, got %q", reqs[0].lines)
	}
	if _, message := parseHTTPFrame(t, reqs[0].lines[0]); !strings.Contains(message, "tick") {
		t.Errorf("message=%q want 含 tick", message)
	}
}

// NDJSON 帧完整性与落盘字段契约：批内单 \n 分隔、无空行、无尾随空行；每帧有且仅有
// src+message 两键，message 为 text 版式单行文本（时间/级别/msg/attrs 全量自包含），
// 帧 JSON 源内无裸换行（含 \n 的消息先被渲染器转义为字面量、再被 JSON 编码为 \\n）。
func TestHTTP_NDJSONFraming(t *testing.T) {
	m := newHTTPMock(t)
	h, _ := newTestHTTPHandler(t, m.url, 3)

	rec := slog.NewRecord(time.Now(), Warn, "multi\nline", 0)
	rec.AddAttrs(slog.String("k", "v"), slog.Group("db", slog.String("table", "users"), slog.Int("rows", 3)))
	_ = h.Handle(t.Context(), rec)
	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Error, "second", 0))
	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Debug, "third", 0))

	// 原始体（解压后）用于断言帧分隔与收尾：NDJSON 惯例每行均以 \n 结束（发送前补齐）
	reqs := m.waitReqs(1, 3*time.Second)
	rawLines := reqs[0].lines
	body := reqs[0].body
	if strings.Contains(body, "\n\n") {
		t.Errorf("批内不应有连续换行（多余空行）: %q", body)
	}
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("批体应以单个换行收尾（NDJSON 惯例）: %q", body)
	}
	if strings.HasSuffix(body, "\n\n") {
		t.Errorf("收尾不应有两个换行（多余空行）: %q", body)
	}
	if !strings.HasSuffix(strings.TrimSuffix(body, "\n"), "}") {
		t.Errorf("最后一个非空行应以 JSON 对象收尾: %q", body)
	}
	if n := strings.Count(body, "\n"); n != 3 {
		t.Errorf("换行数应等于条数（3 条 → 3 个 \\n）, got %d in %q", n, body)
	}
	if n := len(strings.Split(strings.TrimSuffix(body, "\n"), "\n")); n != 3 {
		t.Errorf("按 \\n 切分后行数应==条数 3, got %d", n)
	}
	if len(rawLines) != 3 {
		t.Fatalf("应收到 3 帧, got %d: %q", len(rawLines), rawLines)
	}
	for i, line := range rawLines {
		if strings.ContainsAny(line, "\n\r") {
			t.Errorf("第 %d 帧含裸换行: %q", i+1, line)
		}
		if strings.TrimSpace(line) == "" {
			t.Errorf("第 %d 帧为空行（批内不应有多余 \\n）", i+1)
		}
	}

	// 帧形：两键 src+message；message 自包含 text 版式全文（时间/级别/正文/属性）
	messages := make([]string, 3)
	for i, line := range rawLines {
		_, messages[i] = parseHTTPFrame(t, line)
	}

	// 第 1 帧：Warn + 含换行消息（落盘正文中 \n 为字面量两字符，属预期）+ k=v + 分组展平
	if rest := assertFrameTimePrefix(t, messages[0]); !strings.HasPrefix(rest, "WARN ") {
		t.Errorf("第 1 帧级别段错误（time 后应为 WARN）: %q", messages[0])
	}
	if !strings.Contains(messages[0], `multi\nline`) {
		t.Errorf("第 1 帧 message=%q 应含渲染器转义后的字面量 multi\\nline", messages[0])
	}
	for _, want := range []string{"k=v", "db.table=users", "db.rows=3"} {
		if !strings.Contains(messages[0], want) {
			t.Errorf("第 1 帧 message=%q 缺属性 %q", messages[0], want)
		}
	}
	if !strings.Contains(messages[1], "ERRO") || !strings.Contains(messages[1], "second") {
		t.Errorf("第 2 帧错误: %q", messages[1])
	}
	if !strings.Contains(messages[2], "DEBU") || !strings.Contains(messages[2], "third") {
		t.Errorf("第 3 帧错误: %q", messages[2])
	}
}

// src 缺省链（对端落盘归属）：显式 Src > Options.Service（newHTTPHandler service 形参）
// > os.Hostname()。白名单防护：[a-zA-Z0-9._-] 之外字符构建期 sanitize 为 '-'（保证非空、
// 确定性、防对端落盘文件名污染——实测含 "/.." 会 200 但文件名被污染）。
func TestHTTP_SrcFallbackChain(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("os.Hostname: %v", err)
	}
	hostSanitized := sanitizeHTTPSrc(host)
	cases := []struct {
		name    string
		src     string // HTTPSettings.Src
		service string // newHTTPHandler service 形参（Options.Service 链路）
		want    string
	}{
		{"显式 Src 优先", "explicit-svc", "pay-svc", "explicit-svc"},
		{"Src 空回退 Service", "", "pay-svc", "pay-svc"},
		{"皆空回退 os.Hostname（经白名单化）", "", "", hostSanitized},
		{"越界字符 sanitize 为连字符", "a/b ..c 中", "", "a-b-..c----"},
		{"中文 Src 全量替换", "服务 名", "", "----------"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newHTTPMock(t)
			h, s := newHTTPHandler(&HTTPSettings{
				URL: m.url, BatchSize: 1, FlushInterval: 10 * time.Second, Timeout: 2 * time.Second,
				Src: c.src,
			}, c.service, slog.LevelDebug, false)
			s.retryBase = 10 * time.Millisecond
			s.stderr = &stderrBuf{}
			defer s.Close()

			_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "m", 0))
			reqs := m.waitReqs(1, 3*time.Second)
			gotSrc, message := parseHTTPFrame(t, reqs[0].lines[0])
			if gotSrc != c.want {
				t.Errorf("src=%q want %q", gotSrc, c.want)
			}
			if !httpSrcWhitelistRE.MatchString(gotSrc) {
				t.Errorf("帧 src=%q 含白名单 [a-zA-Z0-9._-] 之外字符", gotSrc)
			}
			if !strings.Contains(message, "m") {
				t.Errorf("message=%q want 含正文 m", message)
			}
		})
	}
}

// sanitizeHTTPSrc 纯函数语义锁定：合规名原样；越界字节逐字节替换为 '-'（不删除、
// 字节数不变、UTF-8 自同步）；空串回退 '-' 保证帧 src 非空；路径注入样本收敛到白名单。
func TestSanitizeHTTPSrc(t *testing.T) {
	cases := []struct{ in, want string }{
		{"order-svc", "order-svc"},           // 合规原样
		{"a.b_c-D9", "a.b_c-D9"},             // 含全部白名单特殊字符
		{"", "-"},                            // 空串 → 非空保证
		{"/../etc/passwd", "-..-etc-passwd"}, // 对端实测污染样本 → 白名单化（200+污染不再可能）
		{"a/b ..c 中", "a-b-..c----"},         // 斜杠/空格/多字节（中=3 字节→3 个 '-'）
		{"服务 名", "----------"},               // 全越界：3+3+1+3 字节 → 10 个 '-'，非空
	}
	for _, c := range cases {
		if got := sanitizeHTTPSrc(c.in); got != c.want {
			t.Errorf("sanitizeHTTPSrc(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// Enabled 与 inner 同源：低于门槛的级别经 slog.Logger 链路即被拦下，不进缓冲批。
func TestHTTP_EnabledLevelGate(t *testing.T) {
	m := newHTTPMock(t)
	h, hs := newHTTPHandler(&HTTPSettings{
		URL: m.url, BatchSize: 10, FlushInterval: 50 * time.Millisecond, Timeout: time.Second,
	}, "", slog.LevelInfo, false)
	hs.stderr = &stderrBuf{}
	defer hs.Close()

	if h.Enabled(t.Context(), Debug) {
		t.Error("level=Info 时 Debug 应被门槛拦下")
	}
	if !h.Enabled(t.Context(), Error) {
		t.Error("level=Info 时 Error 应放行")
	}

	lg := slog.New(h) // Logger 在 Handle 前先查 Enabled
	lg.Debug("drop-me")
	time.Sleep(200 * time.Millisecond)
	if m.reqCount() != 0 {
		t.Errorf("低于门槛的 record 不应被投递, got %d reqs", m.reqCount())
	}

	lg.Info("kept")
	reqs := m.waitReqs(1, 3*time.Second)
	if _, message := parseHTTPFrame(t, reqs[0].lines[0]); !strings.Contains(message, "kept") {
		t.Errorf("message=%q want 含 kept", message)
	}
}

// WithAttrs / WithGroup 派生：组外/组内预设属性与 record 属性按 text 版式（分组以
// 点前缀展平）拼入 message，帧仍为 src+message 两键。
func TestHTTP_WithAttrsAndGroup(t *testing.T) {
	m := newHTTPMock(t)
	h, _ := newTestHTTPHandler(t, m.url, 1)

	rec := slog.NewRecord(time.Now(), Info, "derived", 0)
	rec.AddAttrs(slog.String("rk", "rv"))
	dh := h.WithAttrs([]slog.Attr{slog.String("svc", "pay")}).
		WithGroup("g").
		WithAttrs([]slog.Attr{slog.String("inner", "iv")})
	_ = dh.Handle(t.Context(), rec)

	reqs := m.waitReqs(1, 3*time.Second)
	_, message := parseHTTPFrame(t, reqs[0].lines[0])
	if !strings.Contains(message, "derived") {
		t.Errorf("message=%q 缺 record 正文 derived", message)
	}
	if !strings.Contains(message, "svc=pay") {
		t.Errorf("WithAttrs 组外预设属性丢失: %q", message)
	}
	for _, want := range []string{"g.inner=iv", "g.rk=rv"} {
		if !strings.Contains(message, want) {
			t.Errorf("message=%q 缺组内属性 %q", message, want)
		}
	}
}

// ---- 请求头 ----

func TestHTTP_HeadersAndContentType(t *testing.T) {
	m := newHTTPMock(t)
	h, _ := newHTTPHandler(&HTTPSettings{
		URL: m.url, BatchSize: 1, FlushInterval: 10 * time.Second, Timeout: 2 * time.Second,
		Headers: map[string]string{"Authorization": "Bearer tok-42", "X-Extra": "e1"},
	}, "", slog.LevelDebug, false)
	h.s.stderr = &stderrBuf{}
	defer h.s.Close()

	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "hdr", 0))
	reqs := m.waitReqs(1, 3*time.Second)
	got := reqs[0]
	if got.contentType != "application/json" {
		t.Errorf("Content-Type=%q want application/json", got.contentType)
	}
	if got.auth != "Bearer tok-42" {
		t.Errorf("Authorization=%q want Bearer tok-42（用户头透传）", got.auth)
	}
	if got.extra != "e1" {
		t.Errorf("X-Extra=%q want e1", got.extra)
	}
	if got.contentEncoding != "" {
		t.Errorf("未开 Gzip 不应有 Content-Encoding, got %q", got.contentEncoding)
	}
}

// 用户头可覆写默认 Content-Type（实现按「用户头最后 Set」）。
func TestHTTP_UserHeaderOverridesContentType(t *testing.T) {
	m := newHTTPMock(t)
	h, _ := newHTTPHandler(&HTTPSettings{
		URL: m.url, BatchSize: 1, FlushInterval: 10 * time.Second, Timeout: 2 * time.Second,
		Headers: map[string]string{"Content-Type": "application/x-ndjson"},
	}, "", slog.LevelDebug, false)
	h.s.stderr = &stderrBuf{}
	defer h.s.Close()

	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "ov", 0))
	reqs := m.waitReqs(1, 3*time.Second)
	if reqs[0].contentType != "application/x-ndjson" {
		t.Errorf("Content-Type=%q want 用户覆写生效", reqs[0].contentType)
	}
}

// ---- Gzip ----

func TestHTTP_Gzip(t *testing.T) {
	m := newHTTPMock(t)
	h, _ := newHTTPHandler(&HTTPSettings{
		URL: m.url, BatchSize: 2, FlushInterval: 10 * time.Second, Timeout: 2 * time.Second, Gzip: true,
	}, "", slog.LevelDebug, false)
	h.s.stderr = &stderrBuf{}
	defer h.s.Close()

	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "g1", 0))
	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "g2", 0))

	reqs := m.waitReqs(1, 3*time.Second)
	if !reqs[0].gzipped {
		t.Fatal("Gzip=true 应置 Content-Encoding: gzip")
	}
	if reqs[0].contentType != "application/json" {
		t.Errorf("Content-Type=%q want application/json", reqs[0].contentType)
	}
	if len(reqs[0].lines) != 2 {
		t.Fatalf("gunzip 后应得 2 行, got %q", reqs[0].lines)
	}
	_, message0 := parseHTTPFrame(t, reqs[0].lines[0])
	_, message1 := parseHTTPFrame(t, reqs[0].lines[1])
	if !strings.Contains(message0, "g1") || !strings.Contains(message1, "g2") {
		t.Errorf("gunzip 内容错误: %q", reqs[0].lines)
	}
	// 非空 body 均经 post 补尾换行 → 压缩的是**加尾后**的完整体（无旁路）
	b := reqs[0].body
	if !strings.HasSuffix(b, "\n") {
		t.Errorf("gzip 路径也应对加尾后的完整体压缩, got %q", b)
	}
	if strings.HasSuffix(b, "\n\n") || strings.Contains(b, "\n\n") {
		t.Errorf("gzip 批体不应含多余空行, got %q", b)
	}
	if n := strings.Count(b, "\n"); n != 2 {
		t.Errorf("gzip 批体换行数应==条数 2, got %d in %q", n, b)
	}
}

// ---- 重试与丢弃 ----

// 前 2 次 500、第 3 次 200 → 服务端收到同一批的 3 次尝试，批内容逐字节一致。
func TestHTTP_RetrySameBatch(t *testing.T) {
	m := newHTTPMockOpts(t, httpMockOpts{failures: 2})
	h, _ := newTestHTTPHandler(t, m.url, 2)

	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "r1", 0))
	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "r2", 0))

	reqs := m.waitReqs(3, 3*time.Second)
	if reqs[0].code != http.StatusInternalServerError || reqs[1].code != http.StatusInternalServerError ||
		reqs[2].code != http.StatusOK {
		t.Fatalf("应答码序列错误: %d/%d/%d", reqs[0].code, reqs[1].code, reqs[2].code)
	}
	for i := range 3 {
		if got := len(reqs[i].lines); got != 2 {
			t.Fatalf("第 %d 次尝试应含 2 行, got %d", i+1, got)
		}
		if reqs[i].lines[0] != reqs[0].lines[0] || reqs[i].lines[1] != reqs[0].lines[1] {
			t.Errorf("第 %d 次尝试的批内容与首次不一致（重试应发整批）: %q", i+1, reqs[i].lines)
		}
	}
	if d := h.s.droppedCount(); d != 0 {
		t.Errorf("第 3 次成功后不应有丢弃, got %d", d)
	}
}

// 重试耗尽 → 整批丢弃 + dropped 按条累计 + 同因告警限流（每 5s ≤1 条）。
func TestHTTP_RetryExhaustedDrops(t *testing.T) {
	m := newHTTPMockOpts(t, httpMockOpts{failures: 999})
	h, s := newTestHTTPHandler(t, m.url, 2)

	// 批 1：2 条 → 3 次尝试全 500 → 丢 2 条
	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "a1", 0))
	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "a2", 0))
	if d := waitDropped(t, s, 2, 3*time.Second); d != 2 {
		t.Errorf("批 1 失败后 dropped=%d want 2（按条累计）", d)
	}
	// 等重试链彻底落定（3 次尝试 + 退避）后 mock 不再有请求
	if n := m.waitIdle(200*time.Millisecond, 3*time.Second); n != 3 {
		t.Errorf("批 1 应尝试 3 次, got %d reqs", n)
	}

	// 批 2：同因再失败 → 限流：告警仍只 1 条，dropped 累计到 4
	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "b1", 0))
	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "b2", 0))
	if d := waitDropped(t, s, 4, 3*time.Second); d != 4 {
		t.Errorf("批 2 失败后 dropped=%d want 4", d)
	}

	warns := s.stderr.(*stderrBuf).String()
	if n := strings.Count(warns, "logger: http"); n != 1 {
		t.Errorf("同一原因告警应限流为 1 条, got %d:\n%s", n, warns)
	}
	if !strings.Contains(warns, "dropping 2 events") {
		t.Errorf("告警应含丢弃条数, got: %s", warns)
	}
}

// 4xx（排除 408/429）为确定性失败：不重试，立即丢弃 + 限流告警（文案含 status）；
// 408 / 429 / 5xx 属暂时性失败：维持 3 次指数退避。
func TestHTTP_StatusRetryPolicy(t *testing.T) {
	cases := []struct {
		name string
		code int
		want int // 期望的服务端收件次数
	}{
		{"400 坏帧不重试", http.StatusBadRequest, 1},
		{"404 不重试", http.StatusNotFound, 1},
		{"429 应重试", http.StatusTooManyRequests, 3},
		{"408 应重试", http.StatusRequestTimeout, 3},
		{"500 应重试", http.StatusInternalServerError, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newHTTPMockOpts(t, httpMockOpts{code: c.code})
			h, s := newTestHTTPHandler(t, m.url, 2)

			_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "p1", 0))
			_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "p2", 0))

			reqs := m.waitReqs(c.want, 3*time.Second)
			// 落定后静默观察：不重试的用例不得再有第二次请求
			time.Sleep(300 * time.Millisecond)
			if got := m.reqCount(); got != c.want {
				t.Errorf("状态码 %d: 服务端收件次数=%d want %d", c.code, got, c.want)
			}
			for _, r := range reqs {
				if r.code != c.code {
					t.Errorf("mock 应答码=%d want %d", r.code, c.code)
				}
			}

			// 两种失败都丢弃整批、按条计 dropped
			if d := waitDropped(t, s, 2, 2*time.Second); d != 2 {
				t.Errorf("dropped=%d want 2（按条累计）", d)
			}
			warns := s.stderr.(*stderrBuf).String()
			if n := strings.Count(warns, "logger: http"); n != 1 {
				t.Errorf("告警应限流为 1 条, got %d:\n%s", n, warns)
			}
			if !strings.Contains(warns, fmt.Sprintf("%d", c.code)) {
				t.Errorf("告警文案应含状态码 %d, got:\n%s", c.code, warns)
			}
		})
	}
}

// inflight 槽被慢端点长期占用时：持续写入只丢新批、不 panic，且 Handle 不阻塞。
func TestHTTP_InflightBusyDropsNewBatches(t *testing.T) {
	m := newHTTPMockOpts(t, httpMockOpts{delay: 400 * time.Millisecond})
	h, s := newTestHTTPHandler(t, m.url, 1)

	start := time.Now()
	for range 20 {
		_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "burst", 0))
	}
	elapsed := time.Since(start)
	// 服务端每条 400ms：若 Handle 内联做网络 IO，20 条至少要 8s；纯缓冲应远小于该值
	if elapsed > 100*time.Millisecond {
		t.Errorf("Handle 疑似被网络 IO 阻塞: elapsed=%v", elapsed)
	}

	if d := waitDropped(t, s, 1, 3*time.Second); d == 0 {
		t.Errorf("inflight 占用期间应有丢弃计数, got %d", d)
	}
	if got := m.reqCount(); got >= 20 {
		t.Errorf("溢出批应被丢弃而非排队, got %d reqs", got)
	}
	// 落定后仍有告警且限流
	warns := s.stderr.(*stderrBuf).String()
	if n := strings.Count(warns, "logger: http"); n == 0 {
		t.Error("inflight 丢弃应有至少一条限流告警")
	} else if n > 3 {
		t.Errorf("告警应限流, got %d:\n%s", n, warns)
	}
}

// ---- Close 语义 ----

// 写不满批 → Close 把残余缓冲批最后换出并送达（一次快速尝试、不重试）。
func TestHTTP_CloseFlushesRemaining(t *testing.T) {
	m := newHTTPMock(t)
	h, s := newTestHTTPHandler(t, m.url, 100) // 永不满批；flush=10s 不会到点

	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "tail1", 0))
	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "tail2", 0))

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reqs := m.waitReqs(1, 3*time.Second)
	if len(reqs[0].lines) != 2 {
		t.Fatalf("Close 应把 2 条残余批送达, got %q", reqs[0].lines)
	}
	if _, message := parseHTTPFrame(t, reqs[0].lines[0]); !strings.Contains(message, "tail1") {
		t.Errorf("收尾批内容错误: %q", reqs[0].lines)
	}
	// worker 已停：stopped 通道应已关闭
	select {
	case <-s.stopped:
	default:
		t.Error("Close 返回后发送 worker 应已退出（防 goroutine 泄漏）")
	}
}

// Close 后 Handle 静默丢弃、不再有任何网络 IO；重复 Close 幂等。
func TestHTTP_CloseSilentAndIdempotent(t *testing.T) {
	m := newHTTPMock(t)
	h, s := newTestHTTPHandler(t, m.url, 100)

	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "before", 0))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	m.waitReqs(1, 3*time.Second)

	for range 5 {
		if err := h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "after", 0)); err != nil {
			t.Errorf("Close 后 Handle 应返回 nil, got %v", err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := m.reqCount(); n != 1 {
		t.Errorf("Close 后不应再有请求, got %d", n)
	}
	if err := s.Close(); err != nil {
		t.Errorf("重复 Close 应返回 nil, got %v", err)
	}
}

// ---- configOptions 映射 ----

func TestConfigOptionsHTTPMapping(t *testing.T) {
	cfg := Config{
		Outputs: OutputsConfig{
			HTTP: &HTTPConfig{
				URL:           "https://collector.internal:8686/v1/logs",
				Src:           "order-svc",
				Headers:       map[string]string{"Authorization": "Bearer x"},
				BatchSize:     7,
				FlushInterval: "250ms",
				Timeout:       "3s",
				Gzip:          true,
			},
		},
	}
	o := buildOptions(configOptions(cfg)...)
	if o.HTTP == nil {
		t.Fatal("configOptions 应映射 HTTP 节点 → HTTPSettings")
	}
	s := o.HTTP
	if s.URL != "https://collector.internal:8686/v1/logs" {
		t.Errorf("URL=%q", s.URL)
	}
	if s.Src != "order-svc" {
		t.Errorf("Src=%q want order-svc", s.Src)
	}
	if s.Headers["Authorization"] != "Bearer x" {
		t.Errorf("Headers=%v want 含 Authorization", s.Headers)
	}
	if s.BatchSize != 7 {
		t.Errorf("BatchSize=%d want 7", s.BatchSize)
	}
	if s.FlushInterval != 250*time.Millisecond {
		t.Errorf("FlushInterval=%v want 250ms", s.FlushInterval)
	}
	if s.Timeout != 3*time.Second {
		t.Errorf("Timeout=%v want 3s", s.Timeout)
	}
	if !s.Gzip {
		t.Error("Gzip 应为 true")
	}
}

// 空字符串时长/零值 BatchSize 走 WithHTTP 默认（100 / 500ms / 5s）。
func TestConfigOptionsHTTPDefaults(t *testing.T) {
	cfg := Config{Outputs: OutputsConfig{HTTP: &HTTPConfig{URL: "http://127.0.0.1:8080/px"}}}
	o := buildOptions(configOptions(cfg)...)
	if o.HTTP == nil {
		t.Fatal("HTTP 节点应映射")
	}
	if o.HTTP.BatchSize != 100 {
		t.Errorf("零值 BatchSize 经 WithHTTPBatchSize(0) → 保留默认 100, got %d", o.HTTP.BatchSize)
	}
	if o.HTTP.FlushInterval != 500*time.Millisecond {
		t.Errorf("空 FlushInterval 应回退默认 500ms, got %v", o.HTTP.FlushInterval)
	}
	if o.HTTP.Timeout != 5*time.Second {
		t.Errorf("空 Timeout 应回退默认 5s, got %v", o.HTTP.Timeout)
	}
}

// ---- Init 校验 ----

func TestInitHTTPValidation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantSub string
	}{
		{
			name:    "URL 空",
			cfg:     Config{Outputs: OutputsConfig{HTTP: &HTTPConfig{}}},
			wantSub: "requires non-empty url",
		},
		{
			name:    "URL scheme 非法",
			cfg:     Config{Outputs: OutputsConfig{HTTP: &HTTPConfig{URL: "ftp://h/p"}}},
			wantSub: "unsupported http url scheme",
		},
		{
			name:    "URL 缺 scheme（host:port/path 形态）",
			cfg:     Config{Outputs: OutputsConfig{HTTP: &HTTPConfig{URL: "127.0.0.1:8080/v1/logs"}}},
			wantSub: "invalid http url",
		},
		{
			name:    "BatchSize 负数",
			cfg:     Config{Outputs: OutputsConfig{HTTP: &HTTPConfig{URL: "http://h/p", BatchSize: -1}}},
			wantSub: "invalid http batch_size",
		},
		{
			name:    "Timeout 非法",
			cfg:     Config{Outputs: OutputsConfig{HTTP: &HTTPConfig{URL: "http://h/p", Timeout: "5 seconds"}}},
			wantSub: "invalid http timeout",
		},
		{
			name:    "Timeout 负值",
			cfg:     Config{Outputs: OutputsConfig{HTTP: &HTTPConfig{URL: "http://h/p", Timeout: "-1s"}}},
			wantSub: "invalid http timeout",
		},
		{
			name:    "FlushInterval 非法",
			cfg:     Config{Outputs: OutputsConfig{HTTP: &HTTPConfig{URL: "http://h/p", FlushInterval: "soon"}}},
			wantSub: "invalid http flush_interval",
		},
		{
			name:    "FlushInterval 负值",
			cfg:     Config{Outputs: OutputsConfig{HTTP: &HTTPConfig{URL: "http://h/p", FlushInterval: "-5ms"}}},
			wantSub: "invalid http flush_interval",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := withRestoreDefaultInternal(t)
			err := Init(c.cfg)
			if err == nil {
				t.Fatalf("非法配置应报错: %+v", c.cfg.Outputs.HTTP)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误应含 %q, got: %v", c.wantSub, err)
			}
			if DefaultLogger != orig {
				t.Error("校验失败不得改动 DefaultLogger")
			}
		})
	}
}

// 多个非法项合并上报（errors.Join）。
func TestInitHTTPValidationMerged(t *testing.T) {
	withRestoreDefaultInternal(t)
	err := Init(Config{Outputs: OutputsConfig{HTTP: &HTTPConfig{
		URL:           "syslog://h/p",
		BatchSize:     -5,
		Timeout:       "nope",
		FlushInterval: "-1s",
	}}})
	if err == nil {
		t.Fatal("应报错")
	}
	joined := err.Error()
	for _, sub := range []string{"unsupported http url scheme", "invalid http batch_size", "invalid http timeout", "invalid http flush_interval"} {
		if !strings.Contains(joined, sub) {
			t.Errorf("合并错误缺 %q, got: %v", sub, joined)
		}
	}
}

// 合法 URL 但端点不可达不在 Init 报错（连接懒建，与 file / syslog 语义一致）。
func TestInitHTTPUnreachableNoError(t *testing.T) {
	withRestoreDefaultInternal(t)
	t.Cleanup(func() { _ = Close(2 * time.Second) })
	err := Init(Config{Level: "info", Outputs: OutputsConfig{HTTP: &HTTPConfig{
		URL:     "http://127.0.0.1:1/v1/logs", // 基本不可达
		Timeout: "200ms",
	}}})
	if err != nil {
		t.Errorf("不可达端点不应在 Init 报错: %v", err)
	}
}

// File + HTTP 双非法项合并（errors.Join 覆盖两类 sink）。
func TestInitFileAndHTTPErrorsMerged(t *testing.T) {
	withRestoreDefaultInternal(t)
	err := Init(Config{Outputs: OutputsConfig{
		File: &FileConfig{Filename: ""},
		HTTP: &HTTPConfig{URL: "http://h/p", BatchSize: -1},
	}})
	if err == nil {
		t.Fatal("应报错")
	}
	joined := err.Error()
	if !strings.Contains(joined, "requires non-empty filename") || !strings.Contains(joined, "invalid http batch_size") {
		t.Errorf("应同时含 file 黑洞与 http batch_size 错误, got: %v", joined)
	}
}

// ---- 端到端：Init → DefaultLogger → 远端收集端 ----

// Console + HTTP 并存：MultiHandler 扇出，两端各得一份（stdout 有色/无色不影响 http 侧 NDJSON）。
func TestInitConsoleAndHTTPEndToEnd(t *testing.T) {
	withRestoreDefaultInternal(t)
	t.Cleanup(func() { _ = Close(2 * time.Second) })

	m := newHTTPMock(t)

	got := captureStdoutInternal(t, func() {
		if err := Init(Config{
			Level: "info",
			Outputs: OutputsConfig{
				Console: &ConsoleConfig{}, // 节点非 nil 即启用（NoColor 零值=自动判定）
				HTTP: &HTTPConfig{
					URL:       m.url,
					BatchSize: 1,
					Timeout:   "2s",
				},
			},
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		DefaultLogger.Info("fanout-both", "k", "v")
	})

	if !strings.Contains(got, "fanout-both") {
		t.Errorf("显式声明 Console 时 stdout 应仍有输出, got: %q", got)
	}
	reqs := m.waitReqs(1, 5*time.Second)
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}
	src, message := parseHTTPFrame(t, reqs[0].lines[0])
	if src != sanitizeHTTPSrc(host) {
		t.Errorf("src=%q want %q（Src/Service 皆空回退 os.Hostname 经白名单化）", src, sanitizeHTTPSrc(host))
	}
	if !strings.Contains(message, "fanout-both") || !strings.Contains(message, "k=v") {
		t.Errorf("http 侧 message=%q want 含 fanout-both 与 k=v", message)
	}
}

// Init 仅声明 http（无 console）→ NDJSON 批经全链路送达，且不兜底 stdout 控制台。
func TestInitHTTPEndToEnd(t *testing.T) {
	withRestoreDefaultInternal(t)
	t.Cleanup(func() { _ = Close(2 * time.Second) })

	m := newHTTPMock(t)

	got := captureStdoutInternal(t, func() {
		if err := Init(Config{
			Level:   "info",
			Service: "svc-http-e2e",
			Outputs: OutputsConfig{HTTP: &HTTPConfig{
				URL:       m.url,
				Timeout:   "2s",
				BatchSize: 1, // 每条即满批，免去 FlushInterval 等待
			}},
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		DefaultLogger.Info("wire-ndjson", "z", 9)
	})

	// 未声明 console、仅 http → 不兜底 stdout
	if strings.Contains(got, "wire-ndjson") {
		t.Errorf("仅声明 http 时不应兜底 stdout 控制台, got: %q", got)
	}

	reqs := m.waitReqs(1, 5*time.Second)
	if len(reqs[0].lines) != 1 {
		t.Fatalf("应收到 1 帧, got %q", reqs[0].lines)
	}
	src, message := parseHTTPFrame(t, reqs[0].lines[0])
	// Src 未显式配置 → 缺省链回退 Config.Service（落盘归属=服务名，对端契约）
	if src != "svc-http-e2e" {
		t.Errorf("src=%q want svc-http-e2e（Src 空回退 Service）", src)
	}
	if !strings.Contains(message, "wire-ndjson") {
		t.Errorf("message=%q want 含 wire-ndjson", message)
	}
	if !strings.Contains(message, "svc-http-e2e") {
		t.Errorf("message=%q want 含前置 service 值 svc-http-e2e", message)
	}
	if !strings.Contains(message, "z=9") {
		t.Errorf("message=%q want 含属性 z=9", message)
	}
	if reqs[0].contentType != "application/json" {
		t.Errorf("Content-Type=%q want application/json", reqs[0].contentType)
	}
}

// ---- Fatal 生命周期：退出前必须把 http 缓冲批送达（子进程真实退出路径）----

// envFatalChildURL 非空表示当前进程是 TestFatalFlushesHTTPBatch 拉起的子进程：
// 走真实 logger.Fatal（ExitFunc 未注入 → 真 os.Exit(1)），由父进程侧的 mock 服务端裁决是否送达。
const envFatalChildURL = "GADGET_LOGGER_FATAL_CHILD_URL"

func TestFatalFlushesHTTPBatch(t *testing.T) {
	if childURL := os.Getenv(envFatalChildURL); childURL != "" {
		// 子进程：BatchSize 远大于条数、FlushInterval 远超测试时长 →
		// 缓冲批既不满批也不到点，唯一可能的送达路径是 Fatal 内的 sink 释放链
		// （close → httpSink.Close 的收尾快速投递）。
		if err := Init(Config{
			Level: "info",
			Outputs: OutputsConfig{HTTP: &HTTPConfig{
				URL:           childURL,
				BatchSize:     1000,
				FlushInterval: "30s",
				Timeout:       "2s",
			}},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "child Init: %v\n", err)
			os.Exit(9)
		}
		Fatal("fatal-must-arrive", "n", 1) // 包级 Fatal：经 DefaultLogger 记录 → 完整 sink 释放链 → ExitFunc(1)
		fmt.Fprintln(os.Stderr, "child: Fatal returned without exiting")
		os.Exit(8)
	}

	m := newHTTPMock(t)

	cmd := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$", "-test.count", "1")
	cmd.Env = append(os.Environ(), envFatalChildURL+"="+m.url)
	out, err := cmd.CombinedOutput()
	// 子进程须经 ExitFunc(1) 真实退出（非 1 说明 Fatal 未走完或被 panic 打断）
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("子进程应以退出码 1 结束, got err=%v out=\n%s", err, out)
	}

	reqs := m.waitReqs(1, 5*time.Second)
	if len(reqs[0].lines) != 1 {
		t.Fatalf("Fatal 应把未满批的残余送达 1 帧, got %q (child out:\n%s)", reqs[0].lines, out)
	}
	_, message := parseHTTPFrame(t, reqs[0].lines[0])
	if !strings.Contains(message, "fatal-must-arrive") {
		t.Errorf("message=%q want 含 fatal-must-arrive (child out:\n%s)", message, out)
	}
	if !strings.Contains(message, "n=1") {
		t.Errorf("message=%q want 含属性 n=1 (child out:\n%s)", message, out)
	}
	if !strings.Contains(message, "FATA") {
		t.Errorf("message=%q want 含级别词 FATA（text 版式 FatalLevel 渲染）(child out:\n%s)", message, out)
	}
	// 收尾投递为快速单次尝试：不应有重试造成的重复请求
	time.Sleep(300 * time.Millisecond)
	if n := m.reqCount(); n != 1 {
		t.Errorf("Fatal 收尾投递应只发一次, got %d reqs", n)
	}
}
