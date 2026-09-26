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
const syslogTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// syslogWarnInterval 同一失败原因向 stderr 输出告警的最小间隔（限流，防洪泛）。
const syslogWarnInterval = 5 * time.Second

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

	lastWarn map[string]time.Time // reason → 上次告警时间（限流）
	dropped  uint64               // 失败丢弃计数（观测用，见 droppedCount）
}

func (sc *syslogConn) droppedCount() uint64 {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.dropped
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
// 回退（Tag 空→service→"gadget"）；lvl / addSource 与 console/file 同源传入 inner 渲染器。
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
	hostname := settings.Hostname
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
