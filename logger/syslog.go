package logger

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// syslog.go：syslog 后端（slog.Handler），向远端 syslog 服务发送 RFC5424 报文，
// newline 分帧，适配对端 Vector 0.58 syslog source（自动识别 5424）。
//
// 报文格式（单行 + 尾 \n）：
//
//	<PRI>1 <RFC3339毫秒> <HOSTNAME> <APP-NAME> <PROCID> <MSGID> <SD-ID> <MSG>
//
//   - PRI = facility*8 + severity（severity 见 syslogSeverity）；
//   - PROCID = 真实进程号（严禁 procid/msgid/sd 三者同时为 "-"——Vector 实测会拒收）；
//   - MSGID = "-"、SD-ID = "-"（无结构化数据）；
//   - MSG = 单行渲染体，与现有文件 sink 同源复用 newFileHandler（json→slog.NewJSONHandler，
//     text→console 渲染器 NoColor 形态），二者本身保证单行（JSON 转义换行、text 的
//     appendMessageEscaped/引号规则转义），无需自造第二套格式化器。
//
// 连接：懒 dial（首条消息时 net.Dialer+Timeout），写用 SetWriteDeadline(timeout)。
// 断线 / 写超时 → 关闭连接、下次写入重试 dial；失败期间该条丢弃并向 stderr 输出
// 限流告警（同一原因每 5s 最多 1 条，防告警洪泛）。绝不阻塞日志调用方超过 timeout。

// syslogTimeLayout RFC5424 TIMESTAMP 的 RFC3339 带毫秒格式（含时区偏移）。
// 输出恒为 UTC「Z」或 "+HH:MM" 带冒号形态（Go 的 Z07:00 布局），规避对端实测的
// "+0800" 无冒号形态被 Vector syslog source 静默丢帧的坑（对端接入指导）。
const syslogTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// syslogWarnInterval 同一失败原因向 stderr 输出告警的最小间隔（限流，防洪泛）。
const syslogWarnInterval = 5 * time.Second

// syslogMaxFrame 单帧（整条 RFC5424 报文，含尾部 \n）字节上限 = 对端 Vector syslog
// source 实配的 max_length。超限帧对端**静默丢帧**，截断责任在客户端（对端接入指导）：
// 超限时报文 MSG 体截断至整帧恰 ≤ 该值，MSG 尾部保留 syslogTruncMark 标记。
const syslogMaxFrame = 102400

// syslogTruncMark 截断标记（追加在截断后的 MSG 尾部，其字节数计入 syslogMaxFrame 预算）。
const syslogTruncMark = "…[truncated]"

// syslogSeverity 把 slog 级别映射为 RFC5424 severity 编号。
// Debug（及以下，含 trace）→7、Info→6、Warn→4、Error（及以上，含 fatal）→3。
func syslogSeverity(l slog.Level) int {
	switch {
	case l <= Debug:
		return 7 // LOG_DEBUG（trace 归 debug）
	case l <= Info:
		return 6 // LOG_INFO
	case l <= Warn:
		return 4 // LOG_WARNING
	default:
		return 3 // LOG_ERR（error / fatal 走此路径）
	}
}

// facilityNames syslog 标准设施名 → 编号（RFC5424 / BSD syslog）。
// local0..local7 = 16..23；其余为常用标准名。
var facilityNames = map[string]int{
	"kern":         0,
	"user":         1,
	"mail":         2,
	"daemon":       3,
	"auth":         4,
	"syslog":       5,
	"lpr":          6,
	"news":         7,
	"uucp":         8,
	"cron":         9,
	"authpriv":     10,
	"ftp":          11,
	"ntp":          12,
	"security":     13,
	"console":      14,
	"solaris-cron": 15,
	"local0":       16,
	"local1":       17,
	"local2":       18,
	"local3":       19,
	"local4":       20,
	"local5":       21,
	"local6":       22,
	"local7":       23,
}

// ParseSyslogFacility 解析 syslog 设施名 → 编号（大小写不敏感、自动 trim 空格）。
// 未知名 / 空串 → 返回 error（不静默回退）；空串默认回退 user(1) 由调用方（Init 校验 /
// handler 构建）处理，与本函数「非法即报错」的判据解耦。
func ParseSyslogFacility(s string) (int, error) {
	f, ok := facilityNames[strings.ToLower(strings.TrimSpace(s))]
	if !ok {
		return 0, fmt.Errorf("logger: unknown syslog facility %q", s)
	}
	return f, nil
}

// syslogConn 承载共享连接状态（一个 socket）。多个派生 handler（WithAttrs/WithGroup）
// 经指针共享同一 *syslogConn：mutex 串行化「渲染 + 写」，buffer 复用为写入落地缓冲。
type syslogConn struct {
	mu      sync.Mutex
	network string
	address string
	timeout time.Duration
	conn    net.Conn
	closed  bool

	buf bytes.Buffer // 复用的整包落地缓冲（inner 渲染体拷入 pkt 后可安全 Reset）

	lastWarn  map[string]time.Time // reason → 上次告警时间（限流）
	dropped   uint64               // 失败丢弃计数（观测用，见 droppedCount）
	truncated uint64               // 超限截断计数（观测用，见 truncatedCount；确定性策略非故障，不计 dropped、不告警）
}

func (sc *syslogConn) droppedCount() uint64 {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.dropped
}

// truncatedCount 返回累计截断帧数（包内测试观测点，与 droppedCount 同构）。
// 截断是对端单帧上限下的确定性策略（超限必须截断否则整帧被对端静默丢弃），
// 不是故障，故不向 stderr 告警（防洪泛：超长日志每条都截断会产生与日志等量的告警）。
func (sc *syslogConn) truncatedCount() uint64 {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.truncated
}

// warnLocked 限流输出告警（须持 sc.mu）。同一 reason 每 syslogWarnInterval 最多 1 条。
func (sc *syslogConn) warnLocked(reason string, err error) {
	if sc.lastWarn == nil {
		sc.lastWarn = make(map[string]time.Time)
	}
	now := time.Now()
	if last, ok := sc.lastWarn[reason]; ok && now.Sub(last) < syslogWarnInterval {
		return
	}
	sc.lastWarn[reason] = now
	fmt.Fprintf(os.Stderr, "logger: syslog %s to %s://%s failed, dropping: %v\n",
		reason, sc.network, sc.address, err)
}

// writeLocked 写出整包（须持 sc.mu）：懒 dial + 写超时；失败关闭连接（下次重试 dial）、
// 计丢弃、限流告警。绝不阻塞超过 timeout（dial / write 均带 timeout）。
func (sc *syslogConn) writeLocked(pkt []byte) {
	if sc.conn == nil {
		d := net.Dialer{Timeout: sc.timeout}
		conn, err := d.Dial(sc.network, sc.address)
		if err != nil {
			sc.dropped++
			sc.warnLocked("dial", err)
			return
		}
		sc.conn = conn
	}
	_ = sc.conn.SetWriteDeadline(time.Now().Add(sc.timeout))
	if _, err := sc.conn.Write(pkt); err != nil {
		_ = sc.conn.Close()
		sc.conn = nil // 下次写入重试 dial
		sc.dropped++
		sc.warnLocked("write", err)
	}
}

// Close 关闭连接并置 closed（幂等）；此后 Handle 静默丢弃、不再重连写出。
func (sc *syslogConn) Close() error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed {
		return nil
	}
	sc.closed = true
	if sc.conn != nil {
		err := sc.conn.Close()
		sc.conn = nil
		return err
	}
	return nil
}

// syslogHandler 实现 slog.Handler，把每条 record 渲染为一行 RFC5424 报文写出。
// 派生（WithAttrs/WithGroup）拷贝结构体、共享 *syslogConn（含连接与渲染缓冲）与
// inner 渲染器（其 writer 指向 sc.buf）。
type syslogHandler struct {
	sc       *syslogConn
	inner    slog.Handler // MSG 体渲染器（写向 sc.buf），复用 newFileHandler
	level    slog.Leveler // 级别门槛（与 console/file 同源）
	hostname string
	appname  string
	procid   string
	facility int
}

// newSyslogHandler 按 settings 构建 syslog handler + 共享连接状态。service 供 appname
// 回退（Tag 空→service→"gadget"）与 hostname 回退（Hostname 空→service→os.Hostname()）；
// lvl / addSource 与 console/file 同源传入 inner 渲染器。
// 返回的 *syslogConn 应注册到 Close 链（slogLogger.syslogCloser）。
func newSyslogHandler(settings *SyslogSettings, service string, lvl slog.Leveler, addSource bool) (*syslogHandler, *syslogConn) {
	network := settings.Network
	if network != "udp" {
		network = "tcp" // 默认 tcp；非法值已由 Init 拦截，此处仅兜底
	}
	timeout := settings.Timeout
	if timeout <= 0 {
		timeout = defaultSyslogTimeout
	}
	facility := 1 // 默认 user
	if settings.Facility != "" {
		if f, err := ParseSyslogFacility(settings.Facility); err == nil {
			facility = f
		}
	}
	// HOSTNAME 位 = 对端落盘归属（服务名），缺省链：显式 Hostname > service（Options.Service）
	// > os.Hostname()（构建时一次性解析，与 appname 的 service 回退链路同源）。
	hostname := settings.Hostname
	if hostname == "" {
		hostname = service
	}
	if hostname == "" {
		if h, err := os.Hostname(); err == nil {
			hostname = h
		} else {
			hostname = "-"
		}
	}
	appname := settings.Tag
	if appname == "" {
		appname = service
	}
	if appname == "" {
		appname = "gadget"
	}

	sc := &syslogConn{
		network: network,
		address: settings.Address,
		timeout: timeout,
	}

	// MSG 体渲染器：与文件 sink 同源（json→NewJSONHandler，text→console NoColor）。
	handlerOpts := &slog.HandlerOptions{
		Level:     lvl,
		AddSource: addSource,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05.000"))
			}
			return a
		},
	}
	inner := newFileHandler(&sc.buf, settings.Format, handlerOpts)

	h := &syslogHandler{
		sc:       sc,
		inner:    inner,
		level:    lvl,
		hostname: hostname,
		appname:  appname,
		procid:   strconv.Itoa(os.Getpid()),
		facility: facility,
	}
	return h, sc
}

// truncateSyslogFrame 把超限整帧（含尾 \n）的 MSG 体截短至帧长恰 ≤ syslogMaxFrame：
// 保留 header、截断 MSG 体并在其尾部追加 syslogTruncMark 标记、补回尾 \n。
// bodyLen 为 MSG 体在帧内的字节长度（header 占 len(pkt)-bodyLen-1 字节）。
// 截断点若落在 UTF-8 多字节序列中部，回退到最近的 rune 起始处，绝不产出残缺序列。
// header + 标记 + 尾 \n 本身超预算时返回 nil（丢弃信号；header 为固定字段、理论不可达）。
func truncateSyslogFrame(pkt []byte, bodyLen int) []byte {
	hl := len(pkt) - bodyLen - 1 // header 长（"<PRI>1 <ts> <host> <app> <procid> - - "）
	avail := syslogMaxFrame - hl - 1 - len(syslogTruncMark)
	if avail < 0 {
		return nil
	}
	body := pkt[hl : hl+bodyLen]
	cut := avail
	// 回退到 rune 起始处：body[cut] 为截断点后的首字节，延续字节（0b10xxxxxx）则前移；
	// cut 递减至 0 终止（空体 + 标记，帧仍在预算内）。
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	out := make([]byte, 0, hl+cut+len(syslogTruncMark)+1)
	out = append(out, pkt[:hl]...)
	out = append(out, body[:cut]...)
	out = append(out, syslogTruncMark...)
	out = append(out, '\n')
	return out
}

// Enabled 级别门槛判断（与 inner 同源 Leveler）。
func (h *syslogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle 渲染单条 record 为 RFC5424 报文并写出。整程持 sc.mu 串行化「渲染 + 写」，
// 保证 UDP 单包（不会把 header/body 拆成多数据报）与 TCP 不交错。
func (h *syslogHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := h.sc
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.closed {
		return nil // 包级 Close 后静默丢弃、不再重连
	}

	// 1) 渲染 MSG 体到 sc.buf（inner 写一行 + 尾 \n）。
	sc.buf.Reset()
	if err := h.inner.Handle(ctx, r); err != nil {
		return err
	}
	body := bytes.TrimRight(sc.buf.Bytes(), "\n")

	// 2) 组整包（拷入独立 pkt，避免 body 与后续 sc.buf 复用互相影响）。
	pri := h.facility*8 + syslogSeverity(r.Level)
	pkt := make([]byte, 0, 64+len(body))
	pkt = append(pkt, '<')
	pkt = strconv.AppendInt(pkt, int64(pri), 10)
	pkt = append(pkt, '>', '1', ' ')
	pkt = r.Time.AppendFormat(pkt, syslogTimeLayout)
	pkt = append(pkt, ' ')
	pkt = append(pkt, h.hostname...)
	pkt = append(pkt, ' ')
	pkt = append(pkt, h.appname...)
	pkt = append(pkt, ' ')
	pkt = append(pkt, h.procid...)
	pkt = append(pkt, ' ', '-', ' ', '-', ' ') // msgid="-" sd="-" 后接一个空格到 MSG
	pkt = append(pkt, body...)
	pkt = append(pkt, '\n')

	// 2.5) 单帧上限截断（对端 syslog source max_length 实配 102400，超限帧被对端静默
	// 丢弃，截断归客户端）：截 MSG 体使整帧（含尾 \n）恰 ≤ syslogMaxFrame，MSG 尾部保留
	// 截断标记；截断点回退到 UTF-8 rune 起始处，绝不产出残缺多字节序列。
	if len(pkt) > syslogMaxFrame {
		pkt = truncateSyslogFrame(pkt, len(body))
		if pkt == nil {
			// 理论不可达的极端防御：header + 标记 + 尾 \n 本身已超预算（header 固定字段
			// 最长数百字节 ≪ 102400）→ 丢弃该条计 dropped。
			sc.dropped++
			return nil
		}
		// 截断是确定性策略而非故障：只计数（包内 truncatedCount 观测），不告警（防洪泛）。
		sc.truncated++
	}

	// 3) 写出（懒连接 / 失败限流告警丢弃，不返回错误给日志调用方）。
	sc.writeLocked(pkt)
	return nil
}

// WithAttrs 派生：共享连接、内层渲染器追加属性。
func (h *syslogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := *h
	h2.inner = h.inner.WithAttrs(attrs)
	return &h2
}

// WithGroup 派生：共享连接、内层渲染器进入分组。
func (h *syslogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.inner = h.inner.WithGroup(name)
	return &h2
}
