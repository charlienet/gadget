package logger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 本地 mock 接收端 ----

// syslogMock 在 127.0.0.1 上模拟对端 syslog 服务：newline 分帧读取整报文到 frames。
// stop 关闭 listener 与已建连接（模拟接收端下线）；start 在固定端口重新拉起。
type syslogMock struct {
	t      *testing.T
	addr   string
	port   int
	frames chan string

	mu       sync.Mutex
	listener net.Listener
	conns    []net.Conn
}

func newSyslogMock(t *testing.T) *syslogMock {
	t.Helper()
	m := &syslogMock{t: t, frames: make(chan string, 1024)}
	m.start()
	return m
}

func (m *syslogMock) start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listener != nil {
		return
	}
	addr := m.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		m.t.Fatalf("mock listen %s: %v", addr, err)
	}
	m.listener = ln
	m.addr = ln.Addr().String()
	host, pstr, _ := net.SplitHostPort(m.addr)
	m.port, _ = strconv.Atoi(pstr)
	_ = host
	go m.acceptLoop(ln)
}

// acceptLoop 针对一个确定的 listener 运行；stop 关闭该 ln 使 Accept 返回错误、循环退出。
// 不读取 m.listener 字段，避免与 stop() 的字段写竞争（-race）。
func (m *syslogMock) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener 被关闭 → 正常退出
		}
		m.mu.Lock()
		m.conns = append(m.conns, conn)
		m.mu.Unlock()
		go m.readLoop(conn)
	}
}

func (m *syslogMock) readLoop(conn net.Conn) {
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			select {
			case m.frames <- strings.TrimSuffix(line, "\n"):
			default: // frames 缓冲满：丢弃（不影响断言）
			}
		}
		if err != nil {
			return
		}
	}
}

// stop 关闭 listener 与所有已建连接（模拟 Vector 下线）。可再 start 拉起。
func (m *syslogMock) stop() {
	m.mu.Lock()
	ln := m.listener
	conns := m.conns
	m.listener = nil
	m.conns = nil
	m.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

func (m *syslogMock) close() { m.stop() }

// recv 在 timeout 内取一帧，超时返回 false。
func (m *syslogMock) recv(timeout time.Duration) (string, bool) {
	select {
	case f := <-m.frames:
		return f, true
	case <-time.After(timeout):
		return "", false
	}
}

// ---- RFC5424 解析辅助 ----

type rfc5424 struct {
	pri      int
	version  string
	ts       string
	hostname string
	appname  string
	procid   string
	msgid    string
	sd       string
	msg      string
}

// parseRFC5424 解析一行报文（不含尾 \n）；解析失败 t.Fatal。
func parseRFC5424(t *testing.T, line string) rfc5424 {
	t.Helper()
	if !strings.HasPrefix(line, "<") {
		t.Fatalf("报文应以 <PRI> 开头, got %q", line)
	}
	gt := strings.IndexByte(line, '>')
	if gt < 0 {
		t.Fatalf("PRI 缺少 '>' : %q", line)
	}
	priStr := line[1:gt]
	pri, err := strconv.Atoi(priStr)
	if err != nil {
		t.Fatalf("PRI 非数字 %q: %v", priStr, err)
	}
	rest := line[gt+1:] // "1 <ts> <host> <app> <procid> <msgid> <sd> <MSG>"
	fields := strings.SplitN(rest, " ", 8)
	if len(fields) < 8 {
		t.Fatalf("头部字段不足 8 段: %q (got %d)", rest, len(fields))
	}
	return rfc5424{
		pri:      pri,
		version:  fields[0],
		ts:       fields[1],
		hostname: fields[2],
		appname:  fields[3],
		procid:   fields[4],
		msgid:    fields[5],
		sd:       fields[6],
		msg:      fields[7],
	}
}

// newTestSyslogHandler 直接构建 handler（绕过装配层），指向 mock。
func newTestSyslogHandler(m *syslogMock, settings *SyslogSettings) (*syslogHandler, *syslogConn) {
	if settings.Address == "" {
		settings.Address = m.addr
	}
	if settings.Network == "" {
		settings.Network = "tcp"
	}
	if settings.Timeout == 0 {
		settings.Timeout = 2 * time.Second
	}
	return newSyslogHandler(settings, "", slog.LevelDebug, false)
}

// ---- 级别 → PRI 端到端 ----

func TestSyslogTCP_PRIAndHeaders(t *testing.T) {
	m := newSyslogMock(t)
	defer m.close()

	settings := &SyslogSettings{
		Hostname: "test-host",
		Tag:      "my-app",
		Facility: "user", // =1
		Format:   FormatJSON,
	}
	h, sc := newTestSyslogHandler(m, settings)
	defer sc.Close()

	levels := []struct {
		lvl      slog.Level
		severity int
	}{
		{Trace, 7},      // -8 → debug(7)
		{Debug, 7},      // -4
		{Info, 6},       // 0
		{Warn, 4},       // 4
		{Error, 3},      // 8
		{FatalLevel, 3}, // 12 → err(3)
	}
	pid := strconv.Itoa(os.Getpid())

	for _, tc := range levels {
		rec := slog.NewRecord(time.Now(), tc.lvl, "hello", 0)
		if err := h.Handle(t.Context(), rec); err != nil {
			t.Fatalf("Handle(%v): %v", tc.lvl, err)
		}
		line, ok := m.recv(2 * time.Second)
		if !ok {
			t.Fatalf("级别 %v 未收到报文", tc.lvl)
		}
		p := parseRFC5424(t, line)

		wantPri := 1*8 + tc.severity // facility user=1
		if p.pri != wantPri {
			t.Errorf("级别 %v: PRI=%d want %d", tc.lvl, p.pri, wantPri)
		}
		if p.version != "1" {
			t.Errorf("VERSION=%q want 1", p.version)
		}
		if p.hostname != "test-host" {
			t.Errorf("HOSTNAME=%q want test-host", p.hostname)
		}
		if p.appname != "my-app" {
			t.Errorf("APP-NAME=%q want my-app", p.appname)
		}
		if p.procid != pid {
			t.Errorf("PROCID=%q want pid %q", p.procid, pid)
		}
		if p.msgid != "-" {
			t.Errorf("MSGID=%q want -", p.msgid)
		}
		if p.sd != "-" {
			t.Errorf("SD=%q want -", p.sd)
		}
		// Vector 约束：procid/msgid/sd 不可同时为 "-"
		if p.procid == "-" && p.msgid == "-" && p.sd == "-" {
			t.Error("procid/msgid/sd 三者同为 '-'（Vector 会拒收）")
		}
		// 时间戳 RFC3339 毫秒可解析
		if _, err := time.Parse(syslogTimeLayout, p.ts); err != nil {
			t.Errorf("TIMESTAMP %q 无法按 RFC3339 毫秒解析: %v", p.ts, err)
		}
		// MSG 单行且 JSON 可反序列化、含 message
		if strings.ContainsAny(p.msg, "\n\r") {
			t.Errorf("MSG 含裸换行: %q", p.msg)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(p.msg), &body); err != nil {
			t.Fatalf("MSG 非合法 JSON: %v (msg=%q)", err, p.msg)
		}
		if body["msg"] != "hello" {
			t.Errorf("JSON msg=%v want hello", body["msg"])
		}
	}
}

// facility 影响 PRI 高位：local0(16)*8 + info(6) = 134
func TestSyslogTCP_FacilityInPRI(t *testing.T) {
	m := newSyslogMock(t)
	defer m.close()

	h, sc := newTestSyslogHandler(m, &SyslogSettings{Facility: "local0", Format: FormatJSON})
	defer sc.Close()

	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "x", 0))
	line, ok := m.recv(2 * time.Second)
	if !ok {
		t.Fatal("未收到报文")
	}
	p := parseRFC5424(t, line)
	if want := 16*8 + 6; p.pri != want {
		t.Errorf("local0/info: PRI=%d want %d", p.pri, want)
	}
	// 默认 appname 回退链：Tag/Service 皆空 → "gadget"
	if p.appname != "gadget" {
		t.Errorf("APP-NAME 默认回退=%q want gadget", p.appname)
	}
}

// Tag 空、Service 非空 → appname 回退 Service。
func TestSyslogTCP_AppnameFallbackService(t *testing.T) {
	m := newSyslogMock(t)
	defer m.close()

	h, sc := newSyslogHandler(&SyslogSettings{Address: m.addr, Network: "tcp", Timeout: 2 * time.Second, Format: FormatJSON},
		"pay-svc", slog.LevelDebug, false)
	defer sc.Close()

	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "x", 0))
	line, ok := m.recv(2 * time.Second)
	if !ok {
		t.Fatal("未收到报文")
	}
	if p := parseRFC5424(t, line); p.appname != "pay-svc" {
		t.Errorf("APP-NAME 回退 Service=%q want pay-svc", p.appname)
	}
}

// ---- MSG 渲染与现有 json/text sink 同源一致 ----

func TestSyslogTCP_MSGParityWithFileSink(t *testing.T) {
	rec := slog.NewRecord(time.Now(), Info, "parity", 0)
	rec.AddAttrs(slog.String("k", "v"), slog.Int64("n", 42))

	for _, format := range []FileFormat{FormatJSON, FormatText} {
		m := newSyslogMock(t)

		h, sc := newTestSyslogHandler(m, &SyslogSettings{Facility: "user", Format: format})

		// 派生 WithGroup/WithAttrs，与参照 handler 施加同样的派生序列
		dh := h.WithAttrs([]slog.Attr{slog.String("svc", "a")}).WithGroup("g")
		_ = dh.Handle(t.Context(), rec)
		line, ok := m.recv(2 * time.Second)
		sc.Close()
		m.close()
		if !ok {
			t.Fatalf("format=%v 未收到报文", format)
		}
		p := parseRFC5424(t, line)

		// 参照：直接用 newFileHandler 渲染同一 record（同源函数）
		opts := &slog.HandlerOptions{
			Level: slog.LevelDebug,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey && len(groups) == 0 {
					a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05.000"))
				}
				return a
			},
		}
		var buf bytes.Buffer
		ref := newFileHandler(&buf, format, opts).WithAttrs([]slog.Attr{slog.String("svc", "a")}).WithGroup("g")
		if err := ref.Handle(t.Context(), rec); err != nil {
			t.Fatalf("参照渲染失败 format=%v: %v", format, err)
		}
		want := strings.TrimRight(buf.String(), "\n")
		if p.msg != want {
			t.Errorf("format=%v MSG 与文件 sink 渲染不一致:\n got=%q\nwant=%q", format, p.msg, want)
		}
	}
}

// JSON MSG 反序列化含分组嵌套结构。
func TestSyslogTCP_JSONGroupStructure(t *testing.T) {
	m := newSyslogMock(t)
	defer m.close()

	h, sc := newTestSyslogHandler(m, &SyslogSettings{Format: FormatJSON})
	defer sc.Close()

	rec := slog.NewRecord(time.Now(), Info, "grp", 0)
	rec.AddAttrs(slog.Group("db", slog.String("table", "users"), slog.Int("rows", 3)))
	_ = h.Handle(t.Context(), rec)

	line, ok := m.recv(2 * time.Second)
	if !ok {
		t.Fatal("未收到报文")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(parseRFC5424(t, line).msg), &body); err != nil {
		t.Fatalf("JSON 反序列化: %v", err)
	}
	db, ok := body["db"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 db 分组对象: %v", body)
	}
	if db["table"] != "users" || db["rows"].(float64) != 3 {
		t.Errorf("db 分组内容错误: %v", db)
	}
}

// ---- UDP 基本用例 ----

func TestSyslogUDP_Basic(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer pc.Close()

	settings := &SyslogSettings{
		Network:  "udp",
		Address:  pc.LocalAddr().String(),
		Hostname: "uh",
		Tag:      "udp-app",
		Facility: "daemon", // =3
		Format:   FormatJSON,
		Timeout:  2 * time.Second,
	}
	h, sc := newSyslogHandler(settings, "", slog.LevelDebug, false)
	defer sc.Close()

	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Warn, "udp-msg", 0))

	buf := make([]byte, 4096)
	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("udp read: %v", err)
	}
	packet := string(buf[:n])
	if !strings.HasSuffix(packet, "\n") {
		t.Errorf("UDP 报文应以 \\n 结尾, got %q", packet)
	}
	p := parseRFC5424(t, strings.TrimSuffix(packet, "\n"))
	if want := 3*8 + 4; p.pri != want { // daemon(3)*8 + warning(4)
		t.Errorf("UDP PRI=%d want %d", p.pri, want)
	}
	if p.hostname != "uh" || p.appname != "udp-app" {
		t.Errorf("UDP 头部错误: host=%q app=%q", p.hostname, p.appname)
	}
	if !strings.Contains(p.msg, "udp-msg") {
		t.Errorf("UDP MSG 缺内容: %q", p.msg)
	}
}

// ---- 断线重连 ----

func TestSyslogTCP_Reconnect(t *testing.T) {
	m := newSyslogMock(t)
	defer m.close()

	h, sc := newTestSyslogHandler(m, &SyslogSettings{Format: FormatJSON})
	defer sc.Close()

	// 1) 首条到达，建立连接
	_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "before", 0))
	if line, ok := m.recv(2 * time.Second); !ok || !strings.Contains(line, "before") {
		t.Fatalf("连接前首条应到达, got %q ok=%v", line, ok)
	}

	// 2) 接收端下线：关闭 listener + 连接
	m.stop()

	// 捕获 stderr 统计限流告警（同一原因每 5s ≤1 条）
	capture := captureStderr(t)

	// 断线期间写若干条：不 panic、不阻塞（每条受 timeout 约束）、告警有限
	start := time.Now()
	for range 5 {
		_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "during", 0))
	}
	elapsed := time.Since(start)
	// timeout=2s，但 dial 到已关闭的本地端口应被拒绝（快速失败），5 条远小于 5*timeout
	if elapsed > 5*time.Second {
		t.Errorf("断线期间写入疑似阻塞: elapsed=%v", elapsed)
	}

	dropped := sc.droppedCount()
	if dropped == 0 {
		t.Error("断线期间应有丢弃计数")
	}

	warns := capture()
	nWarn := strings.Count(warns, "logger: syslog")
	if nWarn == 0 {
		t.Error("断线期间应有至少一条限流告警")
	}
	if nWarn > 3 {
		t.Errorf("告警应限流（同原因每 5s ≤1），got %d:\n%s", nWarn, warns)
	}

	// 3) 重新拉起接收端 → 后续消息到达
	m.start()
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		_ = h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "after", 0))
		if line, ok := m.recv(500 * time.Millisecond); ok {
			got = line
			if strings.Contains(line, "after") {
				break
			}
		}
	}
	if !strings.Contains(got, "after") {
		t.Fatalf("重连后消息未到达, last=%q", got)
	}
}

// Close 后静默丢弃、不再重连写出。
func TestSyslogTCP_CloseNoReconnect(t *testing.T) {
	m := newSyslogMock(t)
	defer m.close()

	h, sc := newTestSyslogHandler(m, &SyslogSettings{Format: FormatJSON})
	if err := sc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close 后写入应无副作用（返回 nil、不再 dial）
	if err := h.Handle(t.Context(), slog.NewRecord(time.Now(), Info, "x", 0)); err != nil {
		t.Errorf("Close 后 Handle 应返回 nil, got %v", err)
	}
	if sc.conn != nil {
		t.Error("Close 后 conn 应为 nil")
	}
	if _, ok := m.recv(200 * time.Millisecond); ok {
		t.Error("Close 后不应再有报文写出")
	}
	// 幂等
	if err := sc.Close(); err != nil {
		t.Errorf("重复 Close 应返回 nil, got %v", err)
	}
}

// ---- Facility 解析表驱动 ----

func TestParseSyslogFacility(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"kern", 0, false},
		{"user", 1, false},
		{"daemon", 3, false},
		{"auth", 4, false},
		{"syslog", 5, false},
		{"local0", 16, false},
		{"local7", 23, false},
		{"LOCAL0", 16, false},  // 大小写不敏感
		{"  user  ", 1, false}, // trim
		{"", 0, true},          // 空非法（默认由调用方回退）
		{"bogus", 0, true},     // 非法名
		{"kernal", 0, true},    // 非法名
	}
	for _, c := range cases {
		got, err := ParseSyslogFacility(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseSyslogFacility(%q) 应报错, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSyslogFacility(%q) 意外报错: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseSyslogFacility(%q)=%d want %d", c.in, got, c.want)
		}
	}
}

// severity 映射直接断言。
func TestSyslogSeverity(t *testing.T) {
	cases := []struct {
		lvl  slog.Level
		want int
	}{
		{Trace, 7},
		{Debug, 7},
		{Info, 6},
		{Warn, 4},
		{Error, 3},
		{FatalLevel, 3},
	}
	for _, c := range cases {
		if got := syslogSeverity(c.lvl); got != c.want {
			t.Errorf("syslogSeverity(%v)=%d want %d", c.lvl, got, c.want)
		}
	}
}

// ---- 装配层（Init → DefaultLogger → 远端）端到端 ----

// captureStdoutInternal 在 fn 期间把 os.Stdout 重定向到管道并返回捕获内容。
// 须在 rebuild（读取 os.Stdout）之前完成重定向。
func captureStdoutInternal(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	data, _ := io.ReadAll(r)
	os.Stdout = orig
	return string(data)
}

// Init 仅声明 syslog（无 console）→ 报文经全链路送达远端，且不兜底 stdout 控制台。
func TestInitSyslogEndToEnd(t *testing.T) {
	withRestoreDefaultInternal(t)
	t.Cleanup(func() { _ = Close(2 * time.Second) })

	m := newSyslogMock(t)
	defer m.close()

	got := captureStdoutInternal(t, func() {
		if err := Init(Config{
			Level:   "info",
			Service: "svc-e2e",
			Outputs: OutputsConfig{Syslog: &SyslogConfig{
				Address:  m.addr,
				Facility: "local1", // =17
			}},
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}
		DefaultLogger.Info("wire-message", "z", 9)
	})

	// 未声明 console、仅 syslog → 不兜底 stdout
	if strings.Contains(got, "wire-message") {
		t.Errorf("仅声明 syslog 时不应兜底 stdout 控制台, got: %q", got)
	}

	line, ok := m.recv(2 * time.Second)
	if !ok {
		t.Fatal("Init 端到端未收到 syslog 报文")
	}
	p := parseRFC5424(t, line)
	if want := 17*8 + 6; p.pri != want { // local1(17)*8 + info(6)
		t.Errorf("PRI=%d want %d", p.pri, want)
	}
	// Tag 空 → 回退 Config.Service（svc-e2e）经 WithService→Options.Service→appname
	if p.appname != "svc-e2e" {
		t.Errorf("APP-NAME=%q want svc-e2e（Service 回退）", p.appname)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(p.msg), &body); err != nil {
		t.Fatalf("MSG JSON: %v", err)
	}
	if body["msg"] != "wire-message" {
		t.Errorf("MSG msg=%v want wire-message", body["msg"])
	}
}

// ---- configOptions 映射断言 ----

func TestConfigOptionsSyslogMapping(t *testing.T) {
	cfg := Config{
		Outputs: OutputsConfig{
			Syslog: &SyslogConfig{
				Network:  "udp",
				Address:  "127.0.0.1:6514",
				Tag:      "mytag",
				Hostname: "myhost",
				Facility: "local3",
				Format:   "text",
				Timeout:  "3s",
			},
		},
	}
	o := buildOptions(configOptions(cfg)...)
	if o.Syslog == nil {
		t.Fatal("configOptions 应映射 Syslog 节点 → SyslogSettings")
	}
	s := o.Syslog
	if s.Network != "udp" {
		t.Errorf("Network=%q want udp", s.Network)
	}
	if s.Address != "127.0.0.1:6514" {
		t.Errorf("Address=%q want 127.0.0.1:6514", s.Address)
	}
	if s.Tag != "mytag" || s.Hostname != "myhost" {
		t.Errorf("Tag/Hostname=%q/%q want mytag/myhost", s.Tag, s.Hostname)
	}
	if s.Facility != "local3" {
		t.Errorf("Facility=%q want local3", s.Facility)
	}
	if s.Format != FormatText {
		t.Errorf("Format=%v want text", s.Format)
	}
	if s.Timeout != 3*time.Second {
		t.Errorf("Timeout=%v want 3s", s.Timeout)
	}
}

// 空 Timeout 走默认 5s、空 Format → JSON（configOptions 不覆盖 WithSyslog 默认）。
func TestConfigOptionsSyslogDefaults(t *testing.T) {
	cfg := Config{Outputs: OutputsConfig{Syslog: &SyslogConfig{Address: "127.0.0.1:514"}}}
	o := buildOptions(configOptions(cfg)...)
	if o.Syslog == nil {
		t.Fatal("Syslog 节点应映射")
	}
	if o.Syslog.Timeout != 5*time.Second {
		t.Errorf("空 Timeout 应回退默认 5s, got %v", o.Syslog.Timeout)
	}
	if o.Syslog.Format != FormatJSON {
		t.Errorf("空 Format 应为 JSON, got %v", o.Syslog.Format)
	}
}

// ---- Init 校验 ----

// withRestoreDefault 保存并登记恢复 DefaultLogger / slog 默认实例（同包内测试私有实现）。
func withRestoreDefaultInternal(t *testing.T) *slog.Logger {
	t.Helper()
	orig := DefaultLogger
	t.Cleanup(func() {
		DefaultLogger = orig
		slog.SetDefault(orig)
	})
	return orig
}

func TestInitSyslogValidation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantSub string
	}{
		{
			name:    "Address 空",
			cfg:     Config{Outputs: OutputsConfig{Syslog: &SyslogConfig{Address: ""}}},
			wantSub: "requires non-empty address",
		},
		{
			name:    "Network 非法",
			cfg:     Config{Outputs: OutputsConfig{Syslog: &SyslogConfig{Address: "127.0.0.1:514", Network: "sctp"}}},
			wantSub: "unknown syslog network",
		},
		{
			name:    "Facility 非法",
			cfg:     Config{Outputs: OutputsConfig{Syslog: &SyslogConfig{Address: "127.0.0.1:514", Facility: "notafacility"}}},
			wantSub: "unknown syslog facility",
		},
		{
			name:    "Timeout 非法",
			cfg:     Config{Outputs: OutputsConfig{Syslog: &SyslogConfig{Address: "127.0.0.1:514", Timeout: "5 seconds"}}},
			wantSub: "invalid syslog timeout",
		},
		{
			name:    "Format 非法",
			cfg:     Config{Outputs: OutputsConfig{Syslog: &SyslogConfig{Address: "127.0.0.1:514", Format: "yaml"}}},
			wantSub: "unknown file format",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := withRestoreDefaultInternal(t)
			err := Init(c.cfg)
			if err == nil {
				t.Fatalf("非法配置应报错: %+v", c.cfg.Outputs.Syslog)
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
func TestInitSyslogValidationMerged(t *testing.T) {
	withRestoreDefaultInternal(t)
	err := Init(Config{Outputs: OutputsConfig{Syslog: &SyslogConfig{
		Address:  "",
		Network:  "carrier-pigeon",
		Facility: "nope",
		Timeout:  "soon",
	}}})
	if err == nil {
		t.Fatal("应报错")
	}
	joined := err.Error()
	for _, sub := range []string{"requires non-empty address", "unknown syslog network", "unknown syslog facility", "invalid syslog timeout"} {
		if !strings.Contains(joined, sub) {
			t.Errorf("合并错误缺 %q, got: %v", sub, joined)
		}
	}
}

// 合法地址不可达不在 Init 报错（连接懒建）。
func TestInitSyslogUnreachableNoError(t *testing.T) {
	withRestoreDefaultInternal(t)
	t.Cleanup(func() { _ = Close(2 * time.Second) })
	err := Init(Config{Level: "info", Outputs: OutputsConfig{Syslog: &SyslogConfig{
		Address: "127.0.0.1:1", // 基本不可达
	}}})
	if err != nil {
		t.Errorf("不可达地址不应在 Init 报错: %v", err)
	}
}

// File + Syslog 双非法项合并（errors.Join 覆盖两类 sink）。
func TestInitFileAndSyslogErrorsMerged(t *testing.T) {
	withRestoreDefaultInternal(t)
	err := Init(Config{Outputs: OutputsConfig{
		File:   &FileConfig{Filename: ""},
		Syslog: &SyslogConfig{Address: "127.0.0.1:514", Facility: "bad"},
	}})
	if err == nil {
		t.Fatal("应报错")
	}
	joined := err.Error()
	if !strings.Contains(joined, "requires non-empty filename") || !strings.Contains(joined, "unknown syslog facility") {
		t.Errorf("应同时含 file 黑洞与 facility 错误, got: %v", joined)
	}
}

// ---- stderr 捕获辅助 ----

func captureStderr(t *testing.T) func() string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	var buf bytes.Buffer
	t.Cleanup(func() {
		os.Stderr = orig
		_ = w.Close()
		_ = r.Close()
	})
	// drain 读取当前已缓冲的告警（告警由测试 goroutine 同步写，无并发读）。
	return func() string {
		_ = r.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		tmp := make([]byte, 8192)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
			}
			if err != nil {
				break
			}
		}
		return buf.String()
	}
}
