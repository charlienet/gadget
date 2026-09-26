package logger

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// http.go：http 后端（slog.Handler），以 NDJSON 批量 POST 到远端 HTTP 收集端，
// 适配对端 Vector 0.58 `sources.http_server`（codec = json 逐行解码）。
//
// 协议契约（已实测核验，不得偏离）：
//   - method 默认 POST；服务端 path 默认精确匹配 "/"，故 URL 必须是完整端点（含路径）；
//   - 请求体固定 NDJSON：每行一个 JSON 对象、行间单个 \n、**末尾亦以单个 \n 结束**（发送前
//     统一补齐，见 httpSink.post），不依赖对端在 EOF 刷残余的行为差异。
//     严禁发送 v2 批格式 {"logs":[{"event","metadata"}]}（会被当作单个不透明事件）；
//   - 落盘字段契约（对端接入指导 §2/§3.1）：每行帧固定两键
//     {"src":"<服务归属>","message":"<单行文本>"}——src 决定对端落盘文件名（对端字符
//     集 [a-zA-Z0-9._-]，构建时 sanitize：越界字节替换为 '-'，保证非空，防落盘文件名
//     注入/污染）；对端落盘**只保留 message 文本**、其余结构化字段
//     丢弃，故时间/级别/msg/attrs 必须全部拼进 message 自身（经 text 版式渲染器产出单行文本
//     后再 JSON 字符串编码，见 httpHandler.Handle）；
//   - Content-Type 统一发 "application/json"（服务端不强制）；Gzip 开启时置
//     Content-Encoding: gzip（服务端支持解压）；
//   - 成功判据：2xx（服务端 response_code 默认 200）；4xx/5xx 视为失败，其中 4xx（排除
//     408/429）为确定性失败、不重试（见 httpSink.send）；
//   - 语义 at-least-once、无去重：整批重试可能在对端已落库后重复投递（详见 README）；
//   - 服务端解压后有 100MiB/请求上限，客户端批量默认值远小于该上限；
//   - 连接：Vector 默认 300s 回收空闲连接，Go http.Transport 对已被对端关闭的连接自动重拨，
//     无需特殊处理；IdleConnTimeout 取 100s（< 300s）主动丢弃空闲连接避开竞态窗口。
//
// 失败处理：网络错误 / 5xx / 408 / 429 → 整批指数退避重试（含首次共 3 次尝试），最终失败
// 丢弃该批；4xx（排除 408/429）为确定性失败（对端 Vector 对任一坏帧即整批回 400，重试必然
// 同败）→ 不重试、立即丢弃该批。两种失败都向 stderr 输出限流告警（同一原因每 5s 最多 1 条，
// 与 syslog 同一防洪泛策略）；日志调用方 Handle 全程只做「mutex + 渲染 + append」，绝不做网络 IO。

// ---- Option 层（HTTPSettings / WithHTTP / 子选项，与 SyslogSettings 的组织方式对齐）----

// defaultHTTPBatchSize 每次 POST 的默认最大条数（远小于对端 100MiB/请求上限）。
const defaultHTTPBatchSize = 100

// defaultHTTPFlushInterval 不满批时的默认强制投递间隔。
const defaultHTTPFlushInterval = 500 * time.Millisecond

// defaultHTTPTimeout 整请求（含重试的单次尝试）默认超时。
const defaultHTTPTimeout = 5 * time.Second

// HTTPSettings http sink 的可选参数（Options.HTTP：nil=未声明 WithHTTP）。
// 与 WithFile/WithSyslog 惯例一致：WithHTTP 先填默认再应用子选项，最终写入 o.HTTP。
// 各字段语义与默认见 HTTPConfig（Config 层）注释；本结构是 Option 精调层的等价形态。
type HTTPSettings struct {
	URL           string            // 完整 POST 端点（含路径，必填）
	Src           string            // 落盘帧 src 字段（服务归属）；空回退 Options.Service，再空回退 os.Hostname()；越界字符构建时 sanitize 为 '-'
	Headers       map[string]string // 附加请求头（认证等由应用端注入）
	BatchSize     int               // 每次 POST 最大条数；<=0 由 handler 回退默认 100
	FlushInterval time.Duration     // 不满批强制投递间隔；<=0 回退默认 500ms
	Timeout       time.Duration     // 整请求超时；<=0 回退默认 5s
	Gzip          bool              // 请求体 gzip 压缩
}

// WithHTTP 启用 http sink（向远端批量 POST NDJSON）。必须把 o.HTTP 置为非 nil
// （即使不带子选项），再依次应用 opts。url 为完整 POST 端点（含路径，必填）。
// 子选项缺省即默认：BatchSize=100、FlushInterval=500ms、Timeout=5s、Headers=nil、Gzip=false。
func WithHTTP(url string, opts ...HTTPOption) Option {
	return func(o *Options) {
		s := &HTTPSettings{
			URL:           url,
			BatchSize:     defaultHTTPBatchSize,
			FlushInterval: defaultHTTPFlushInterval,
			Timeout:       defaultHTTPTimeout,
		}
		for _, opt := range opts {
			opt(s)
		}
		o.HTTP = s
	}
}

// HTTPOption http sink 的子选项（作用于 HTTPSettings）。
type HTTPOption func(*HTTPSettings)

// WithHTTPHeaders 附加请求头（构建时拷贝，此后调用方改动 map 不影响已建 handler）。
func WithHTTPHeaders(headers map[string]string) HTTPOption {
	return func(s *HTTPSettings) { s.Headers = headers }
}

// WithHTTPSrc 落盘帧的 src 字段（服务归属，决定对端落盘文件名）。空串时由 handler
// 回退链 Src > Options.Service > os.Hostname()（构建时一次性解析）。对端落盘文件名字符
// 集限 [a-zA-Z0-9._-]：越界字节在构建时被 sanitize 为 '-'（见 newHTTPHandler），非空有保证。
func WithHTTPSrc(src string) HTTPOption {
	return func(s *HTTPSettings) { s.Src = src }
}

// WithHTTPBatchSize 每次 POST 的最大条数（<=0 由 handler 回退默认 100）。
func WithHTTPBatchSize(n int) HTTPOption {
	return func(s *HTTPSettings) { s.BatchSize = n }
}

// WithHTTPFlushInterval 不满批时的强制投递间隔（<=0 由 handler 回退默认 500ms）。
func WithHTTPFlushInterval(d time.Duration) HTTPOption {
	return func(s *HTTPSettings) { s.FlushInterval = d }
}

// WithHTTPTimeout 整请求超时（<=0 由 handler 回退默认 5s）。
func WithHTTPTimeout(d time.Duration) HTTPOption {
	return func(s *HTTPSettings) { s.Timeout = d }
}

// WithHTTPGzip 请求体 gzip 压缩（置 Content-Encoding: gzip）。
func WithHTTPGzip(enabled bool) HTTPOption {
	return func(s *HTTPSettings) { s.Gzip = enabled }
}

// ---- 发送节奏常量（不对外配置，见「不做项」）----

const (
	// httpMaxAttempts 单批最多尝试次数（含首次）。
	httpMaxAttempts = 3
	// httpRetryMax 指数退避的间隔上限。
	httpRetryMax = 2 * time.Second
	// httpWarnInterval 同一失败原因向 stderr 告警的最小间隔（与 syslogWarnInterval 同源策略）。
	httpWarnInterval = 5 * time.Second
	// httpIdleConnTimeout 连接池空闲上限：短于 Vector 默认 300s 回收，主动淘汰避开重拨竞态。
	httpIdleConnTimeout = 100 * time.Second
)

// httpRetryBase 指数退避起始间隔（包内常量口径，测试经 sink.retryBase 字段缩短节奏，
// 不改全局变量以免 worker 与测试 goroutine 产生数据竞争）。
const httpRetryBase = 100 * time.Millisecond

// httpBatch 一次 POST 的载荷单元：NDJSON 体 + 条数 + 快速标记。
// fast=true 表示 Close 时的最后收尾批：只尝试一次、不重试（限时 Timeout）。
type httpBatch struct {
	data []byte
	n    int
	fast bool
}

// httpSink 承载 http 后端的共享发送状态（连接池、缓冲批、inflight 槽、统计）。
// 多个派生 handler（WithAttrs/WithGroup）经指针共享同一 *httpSink。
//
// 缓冲模型（保持最简）：
//   - cur：当前缓冲批（NDJSON，帧间以单个 \n 连接、无尾部换行），mu 保护，Handle 只 append；
//   - pending：inflight 槽，至多一批「已换出待发送/正在重试」；换出时若槽被占用，
//     说明上一批仍在重试未落定 → 丢弃新换出的整批（按条计 dropped + 限流告警）；
//   - worker：单 goroutine（构建时启动、Close 必停），从槽取批发送 + ticker 到点换出当前批。
type httpSink struct {
	mu     sync.Mutex
	stderr io.Writer // 告警输出（构建时快照 os.Stderr；测试注入 buffer，避免跨 goroutine 改全局变量）

	url       string
	src       string // 落盘帧 src 字段（构建时解析并 sanitize：Settings.Src > service > os.Hostname()）
	headers   map[string]string
	batch     int
	flush     time.Duration
	timeout   time.Duration
	gzip      bool
	retryBase time.Duration

	client *http.Client

	buf  bytes.Buffer // inner 渲染落地缓冲（mu 保护，同 syslogConn.buf 惯例）
	cur  []byte       // 当前缓冲批（NDJSON 帧拼接，无尾 \n）
	curN int          // 当前缓冲批条数

	hasPending bool      // inflight 槽是否被占用
	pending    httpBatch // inflight 槽内容

	wake    chan struct{} // 通知 worker「槽里有活」（容量 1，非阻塞投递）
	quit    chan struct{} // Close 信号
	stopped chan struct{} // worker 已退出（Close 等待该信号后才关 idle 连接）

	closed   bool
	lastWarn map[string]time.Time // reason → 上次告警时间（限流）
	dropped  uint64               // 失败/溢出丢弃计数（观测用，见 droppedCount）
}

// droppedCount 返回累计丢弃条数（包内测试观测点，与 syslogConn.droppedCount 同构）。
func (s *httpSink) droppedCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// warnLocked 限流输出告警（须持 s.mu）。同一 reason 每 httpWarnInterval 最多 1 条。
// reason 为 "post"（网络/非 2xx 最终失败）或 "inflight"（上一批重试中导致新批溢出丢弃）。
func (s *httpSink) warnLocked(reason string, n int, err error) {
	if s.lastWarn == nil {
		s.lastWarn = make(map[string]time.Time)
	}
	now := time.Now()
	if last, ok := s.lastWarn[reason]; ok && now.Sub(last) < httpWarnInterval {
		return
	}
	s.lastWarn[reason] = now
	w := s.stderr
	if w == nil {
		w = os.Stderr
	}
	if err != nil {
		fmt.Fprintf(w, "logger: http %s to %s failed, dropping %d events: %v\n", reason, s.url, n, err)
		return
	}
	fmt.Fprintf(w, "logger: http %s to %s, dropping %d events\n", reason, s.url, n)
}

// swapLocked 把当前缓冲批换出进 inflight 槽（须持 s.mu），非阻塞：
// 槽被占用（上一批仍在重试未落定）→ 丢弃新换出的整批 + 按条计 dropped + 限流告警。
// 换出成功则置 fast 标记并踢一脚 worker。空批不动作。
func (s *httpSink) swapLocked(fast bool) {
	if len(s.cur) == 0 {
		return
	}
	data, n := s.cur, s.curN
	s.cur, s.curN = nil, 0 // 换出后 cur 归零，日志调用方另起新批

	if s.hasPending {
		s.dropped += uint64(n)
		s.warnLocked("inflight busy", n, nil)
		return
	}
	s.pending = httpBatch{data: data, n: n, fast: fast}
	s.hasPending = true
	select {
	case s.wake <- struct{}{}:
	default: // worker 已被踢过、尚未取走：无需重复通知
	}
}

// takePending 取走 inflight 槽中的批（无批时 ok=false）。
func (s *httpSink) takePending() (httpBatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasPending {
		return httpBatch{}, false
	}
	b := s.pending
	s.pending = httpBatch{}
	s.hasPending = false
	return b, true
}

// run worker 主循环：ticker 到点换出当前批、wake 提示槽里有活、quit 收尾退出。
// 发送（含退避重试）不持 mu：期间 Handle 可继续缓冲新日志。
func (s *httpSink) run() {
	defer close(s.stopped)

	ticker := time.NewTicker(s.flush) // Ticker 随 worker 生命周期创建/停止
	defer ticker.Stop()

	for {
		select {
		case <-s.wake:
		case <-ticker.C:
			s.mu.Lock()
			s.swapLocked(false)
			s.mu.Unlock()
		case <-s.quit:
			// Close：把槽内残留批（含 Close 自己换出的收尾批）最后送一次后退出。
			if b, ok := s.takePending(); ok {
				s.send(b)
			}
			return
		}

		if b, ok := s.takePending(); ok {
			s.send(b)
		}
	}
}

// httpStatusError 非 2xx 应答错误：携带 status code，供 send 判别「确定性失败」。
// 4xx（排除 408/429）重试必然同败——对端 Vector 对任一坏帧即整批回 400，
// 故这类失败直接丢弃该批并告警，不做无谓的 3 次重试。
type httpStatusError struct {
	status int // 应答码
	text   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("unexpected response status %s", e.text)
}

// isPermanentStatus 判定「重试不可能改变结果」的应答码：4xx 客户端错误，
// 但 408 Request Timeout 与 429 Too Many Requests 属暂时性、值得重试。
func isPermanentStatus(status int) bool {
	if status < 400 || status >= 500 {
		return false
	}
	return status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}

// send 发送整批：常规批含首次共 httpMaxAttempts 次尝试、指数退避（retryBase 起倍增至
// httpRetryMax 上限）；收尾批（fast）只尝试一次。单次尝试超时 = timeout，
// 故单批总时限上界 = timeout×尝试次数 + 退避间隔之和（3 次时约 3×timeout + 100ms + 200ms）。
// 4xx（排除 408/429）为确定性失败 → 立即停止重试。
// 最终失败（或确定性失败）→ 丢弃该批 + 按条计 dropped + 限流告警；绝不 panic、绝不无限重试。
func (s *httpSink) send(b httpBatch) {
	attempts := httpMaxAttempts
	if b.fast {
		attempts = 1
	}

	reason := "post"
	delay := s.retryBase
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(delay)
			delay *= 2
			if delay > httpRetryMax {
				delay = httpRetryMax
			}
		}
		err := s.post(b.data)
		if err == nil {
			return
		}
		lastErr = err
		var se *httpStatusError
		if errors.As(err, &se) && isPermanentStatus(se.status) {
			reason = fmt.Sprintf("post %d", se.status) // 如 "post 400"：按状态码分组的限流 reason
			break
		}
	}

	s.mu.Lock()
	s.dropped += uint64(b.n)
	s.warnLocked(reason, b.n, lastErr)
	s.mu.Unlock()
}

// post 单次 POST 请求：per-request context.WithTimeout(timeout) 控超时；
// Gzip 开启时压缩请求体并置 Content-Encoding: gzip。
// 成功判据 2xx；网络错误与非 2xx 均返回 error（非 2xx 为 *httpStatusError，
// 交 send 决定重试/短路丢弃）。
//
// 发送前保证批体以单个 `\n` 收尾：帧在缓冲批内以帧间 `\n` 连接（无尾换行），
// 此处补上尾换行后再压缩/发送，符合 NDJSON 惯例（每行均以 \n 结束），
// 不依赖对端 EOF 刷残余的行为差异。gzip 压缩的是**加尾后**的完整体。
func (s *httpSink) post(ndjson []byte) error {
	if len(ndjson) == 0 {
		return nil // 防御：正常路径（swapLocked 空批不动作）不会投递空批，空体也不该打网络请求
	}
	if ndjson[len(ndjson)-1] != '\n' {
		// 拷一份再加尾：避免就地 append 复用缓冲批的 spare 容量
		ended := make([]byte, 0, len(ndjson)+1)
		ended = append(ended, ndjson...)
		ndjson = append(ended, '\n')
	}

	body := ndjson
	gzipped := false
	if s.gzip {
		var comp bytes.Buffer
		zw := gzip.NewWriter(&comp)
		if _, err := zw.Write(ndjson); err != nil {
			_ = zw.Close()
			return fmt.Errorf("gzip encode: %w", err)
		}
		if err := zw.Close(); err != nil { // 未 Close 的 gzip 流不完整，必须报错
			return fmt.Errorf("gzip close: %w", err)
		}
		body = comp.Bytes()
		gzipped = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	// 用户头最后 Set：允许应用端覆写默认 Content-Type / Content-Encoding（如自定义鉴权、
	// 或对端要求不同的 MIME），本库不做保留头保护。
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		// 排空并关闭响应体：让连接可复用进空闲池（不排空则直接断开，白付重拨代价）
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{status: resp.StatusCode, text: resp.Status}
	}
	return nil
}

// Close 幂等关闭：停止 ticker（随 worker 退出）、把当前缓冲批最后换出并发送一次
// （快速尝试、不重试、限时 Timeout）、等 inflight 落定后关闭空闲连接；
// 此后 Handle 静默丢弃。等待总超时 2×timeout，超时段内残留批丢弃 + 告警。
// 触发点：包级 Close、New/Init 替换默认实例，以及 Fatal（经 slogLogger.close 完整释放链）——
// 后者保证 Fatal 级日志即便未攒满批、未到 FlushInterval 也会在进程退出前尽力送达。
//
// 与 syslogConn.Close 一样返回 error 恒为 nil（尽力语义，残余以 stderr 限流告警暴露），
// 便于直接注册进 slogLogger.close 链。
func (s *httpSink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.swapLocked(true) // 收尾批标 fast：只尝试一次、不重试
	s.mu.Unlock()

	close(s.quit)

	wait := 2 * s.timeout
	select {
	case <-s.stopped:
	case <-time.After(wait):
		// worker 仍在重试（3 次尝试的总时限可超过 2×timeout）：丢弃槽内残留并报告，
		// 不等它——语义对齐 async.Close 的「超时报告残余、调用方不再复用该实例」。
		if b, ok := s.takePending(); ok {
			s.mu.Lock()
			s.dropped += uint64(b.n)
			s.warnLocked("close timeout", b.n, errors.New("inflight batch not settled within close timeout"))
			s.mu.Unlock()
		}
	}

	s.client.CloseIdleConnections()
	return nil
}

// sanitizeHTTPSrc 把 src 收敛到对端落盘文件名白名单 [a-zA-Z0-9._-]：集合外字节一律
// 替换为 '-'（ASCII 白名单对 UTF-8 自同步，多字节字符逐字节替换为等量 '-'，不产生
// 非法 UTF-8）。空串（含全越界再全删的场景不存在——替换非删除，仅输入为空才为空）
// 回退 "-"，保证帧 src 恒非空、确定性。
func sanitizeHTTPSrc(src string) string {
	if src == "" {
		return "-"
	}
	b := []byte(src)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			b[i] = '-'
		}
	}
	return string(b)
}

// httpFrame NDJSON 单行帧（对端落盘字段契约，见文件头注释）：仅 src 与 message 两键，
// 经 encoding/json 紧凑序列化为一行；两字段均走 JSON 字符串转义，防注入且帧内无裸换行。
type httpFrame struct {
	Src     string `json:"src"`
	Message string `json:"message"`
}

// httpHandler 实现 slog.Handler：把每条 record 经 inner 渲染器渲染为一行 NDJSON 帧，
// 追加进 inflight 缓冲批，由 worker 批量 POST。派生（WithAttrs/WithGroup）拷贝结构体、
// 共享 *httpSink 与 inner 渲染器（其 writer 指向 s.buf），与 syslogHandler 同构。
type httpHandler struct {
	s     *httpSink
	inner slog.Handler // 帧正文渲染器（写向 s.buf），复用 newFileHandler(FormatText)：text 版式（console 渲染器 NoColor 形态）
	level slog.Leveler
}

// newHTTPHandler 按 settings 构建 http handler + 共享发送状态（并启动 worker goroutine）。
// service 供帧 src 字段回退（Settings.Src 空→service→os.Hostname()，构建时一次性解析，
// 传入方式与 newSyslogHandler 的 service 形参链路一致）；
// lvl / addSource 与 console/file/syslog 同源传入 inner 渲染器。
// 返回的 *httpSink 应注册到 Close 链（slogLogger.httpCloser），Close 必停 worker。
//
// src 白名单防护（对端落盘文件名字符集 [a-zA-Z0-9._-]）：缺省链解析后统一 sanitize，
// 越界字节替换为 '-'——对端实测 src 含 "/.." 时应答 200 但落盘文件名被污染，故必须
// 在发送前收敛（确定性、保证非空）；Init 层不校验（值可能来自动态 Config.Service），
// 由本 sanitize 兜底。
func newHTTPHandler(settings *HTTPSettings, service string, lvl slog.Leveler, addSource bool) (*httpHandler, *httpSink) {
	batch := settings.BatchSize
	if batch <= 0 {
		batch = defaultHTTPBatchSize // 非法/未设兜底（负值已由 Init 拦截）
	}
	flush := settings.FlushInterval
	if flush <= 0 {
		flush = defaultHTTPFlushInterval
	}
	timeout := settings.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}

	headers := make(map[string]string, len(settings.Headers)) // 构建时拷贝，与调用方解耦
	for k, v := range settings.Headers {
		headers[k] = v
	}

	// 帧 src 缺省链（构建时一次性解析）：显式 Src > service（Options.Service）> os.Hostname()，
	// 解析结果统一 sanitize 为对端白名单字符集（保证非空，见 sanitizeHTTPSrc）。
	src := settings.Src
	if src == "" {
		src = service
	}
	if src == "" {
		if h, err := os.Hostname(); err == nil {
			src = h
		}
	}
	src = sanitizeHTTPSrc(src)

	s := &httpSink{
		stderr:    os.Stderr,
		url:       settings.URL,
		src:       src,
		headers:   headers,
		batch:     batch,
		flush:     flush,
		timeout:   timeout,
		gzip:      settings.Gzip,
		retryBase: httpRetryBase,
		client: &http.Client{
			// 复用单例 Client；超时走 per-request context（不设 Client.Timeout，
			// 避免与 ctx 双超时叠加难以归因）。Transport 自建保守连接池：
			// 单 worker 顺序发送，2 个空闲连接足矣；IdleConnTimeout(100s) 短于
			// Vector 默认 300s 回收窗口，空闲连接由本端先淘汰，对端若已重拨断开，
			// Go 也会对「已关闭连接」自动重拨，无需应用层健康检查。
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     httpIdleConnTimeout,
			},
		},
		wake:    make(chan struct{}, 1),
		quit:    make(chan struct{}),
		stopped: make(chan struct{}),
	}

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
	// 帧正文渲染器与文件 text sink / console 同源（FormatText → console 渲染器 NoColor 形态）：
	// 裸值版式含 time/level/msg/attrs 全量、单行（appendMessageEscaped 已把裸 \n/\r 转义为
	// 字面量两字符），trim 尾 \n 后整体作为帧 message 字段做 JSON 字符串编码。
	// 对端落盘只保留 message 文本，故结构化信息必须全部拼进这一行文本自身。
	inner := newFileHandler(&s.buf, FormatText, handlerOpts)

	go s.run() // 发送 worker 随 handler 构建启动；Close 必停（见 httpSink.Close）

	return &httpHandler{s: s, inner: inner, level: lvl}, s
}

// Enabled 级别门槛判断（与 inner 同源 Leveler）。
func (h *httpHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle 渲染单条 record 为两键帧 {"src":…,"message":…} 并追加进当前缓冲批。
// 全程只做 mutex + 渲染 + append，绝不做网络 IO（网络 IO 在 worker goroutine）；
// 批满则换出踢 worker。message 取 text 版式渲染器输出的单行文本（含时间/级别/msg/attrs）；
// 文本内原有的 \n 字面量两字符经 JSON 编码为 \\n——落盘后 message 含 \n 字面量属预期
// （对端契约要求正文单行），不做特殊处理。
func (h *httpHandler) Handle(ctx context.Context, r slog.Record) error {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil // 包级 Close 后静默丢弃（不记 dropped：属正常关闭语义，非失败丢弃）
	}

	s.buf.Reset()
	if err := h.inner.Handle(ctx, r); err != nil {
		return err
	}
	text := bytes.TrimRight(s.buf.Bytes(), "\n") // text 渲染器每行带尾 \n，帧正文不含换行
	frame, err := json.Marshal(httpFrame{Src: s.src, Message: string(text)})
	if err != nil { // 两字段皆为 string，理论不可达；防御性返回错误、不 panic
		return err
	}
	if len(s.cur) > 0 {
		s.cur = append(s.cur, '\n')
	}
	s.cur = append(s.cur, frame...)
	s.curN++

	if s.curN >= s.batch {
		s.swapLocked(false)
	}
	return nil
}

// WithAttrs 派生：共享发送状态、内层渲染器追加属性。
func (h *httpHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := *h
	h2.inner = h.inner.WithAttrs(attrs)
	return &h2
}

// WithGroup 派生：共享发送状态、内层渲染器进入分组。
func (h *httpHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.inner = h.inner.WithGroup(name)
	return &h2
}
