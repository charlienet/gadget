package logger

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ParseFileFormat 解析文件输出格式字符串（FileConfig.Format → FileFormat 枚举）。
// 空串 → FormatJSON（向后兼容）；"json"/"text"（大小写不敏感、自动 trim 空格）→ 对应枚举；
// 其它非空值 → 返回 error（不静默回退），由 Init 与黑洞校验合并上报。
func ParseFileFormat(s string) (FileFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "json":
		return FormatJSON, nil
	case "text":
		return FormatText, nil
	default:
		return FormatJSON, fmt.Errorf("logger: unknown file format %q (want \"json\" or \"text\")", s)
	}
}

// Init 根据 Config 初始化包级默认 logger，并接受若干精调 Option。
// 级别解析：配置文件优先，环境变量 LOG_LEVEL 兜底。
// New 内部已 slog.SetDefault，故 slog 包级函数自动使用本默认 logger；
// Init 会替换包级 DefaultLogger（旧默认实例在 New 内被关闭，见 default.go）。
//
// 合并顺序：Config 打底生成的 base Option 在前、用户 opts 在后，同名 Option 后者胜——
// 用户可用 WithConsole/WithFile/WithSensitiveKeys 等精调或覆盖 Config 的默认映射（Init(cfg) 仍源码兼容）。
//
// sink 映射（Config.Outputs 节点存在性 → 分组 Option）：Console 节点非 nil → WithConsole；
// File 节点非 nil → WithFile；Syslog 节点非 nil → WithSyslog；HTTP 节点非 nil → WithHTTP；
// 诸 sink 节点皆 nil 时不声明任何 sink Option，由 New 兜底 stdout 控制台。控制台 writer 缺省 os.Stdout。文件输出格式经 ParseFileFormat
// 把 FileConfig.Format 字符串转为 FileFormat 枚举后由 WithFormat 传入（仅 File 节点消费）。
// 控制台颜色经 ConsoleConfig.NoColor 两态映射：false（默认）→ 裸 WithConsole()
// 走自动判定（终端支持 ANSI 即应用、跟随 NO_COLOR、非 TTY 不涂色）；true → WithConsole(WithConsoleColor(false))
// 强制关闭颜色。Config 层不提供强制开色，该能力留在 Option 精调层 WithConsoleColor(true)。
//
// 文件轮换经 FileConfig.Layout 映射：非空→WithDateRotate(layout) 启用按日期轮换、
// 空→维持 lumberjack 按大小轮换。敏感打码为横切能力（console/file 双端生效）：
// Sensitive.Keys 非空→WithSensitiveKeys（追加词、与内置词集合并）、Sensitive.Mask
// 非空→WithSensitiveMask；二者零值均不注入对应 Option（保持现状）。
//
// 返回错误（合并上报，任一失败都不改动 DefaultLogger）：
//   - 黑洞：File 节点存在但合并用户 opts 后文件路径仍为空（用户可用 WithFile(path) 补路径消除）；
//   - 非法格式：FileConfig.Format 为非空未知值（不静默回退）；
//   - syslog：Address 为空（合并用户 opts 后）、Network 非法（仅允许 ""/tcp/udp）、
//     Facility 非法名、Timeout 非空但无法 time.ParseDuration 解析、Format 非法。
//     注意连接是懒建/后台重连，地址不可达不在 Init 报错（与 file 的 lumberjack 惰性 IO 语义一致）。
//   - http：URL 为空（合并用户 opts 后）或无法 url.Parse 解析或 scheme 非 http/https、
//     BatchSize 负数、Timeout / FlushInterval 非空但无法解析或为负值。
//     端点不可达同样不在 Init 报错（首个批次发送时才拨号，与 file / syslog 的懒 IO 语义一致）。
func Init(cfg Config, opts ...Option) error {
	// Config 打底映射抽为纯函数 configOptions（见下），便于映射层单测断言。
	cfgBase := configOptions(cfg)

	// 合并：Config 打底在前、用户 opts 在后（后者覆盖同类项）。
	finalOpts := append(cfgBase, opts...)

	// sink 合法性校验后移：基于合并后的最终 sink 结果判定（用户可用 WithFile/WithSyslog/WithHTTP 补参数
	// 消除黑洞），与格式/枚举/时长解析错误一并 errors.Join 上报，任一失败都不改动 DefaultLogger。
	var problems []error
	if cfg.Outputs.File != nil {
		o := buildOptions(finalOpts...)
		if o.File == "" {
			problems = append(problems, errors.New("logger: file output requires non-empty filename"))
		}
		if _, err := ParseFileFormat(cfg.Outputs.File.Format); err != nil {
			problems = append(problems, err)
		}
	}
	if sc := cfg.Outputs.Syslog; sc != nil {
		problems = append(problems, validateSyslogConfig(sc, buildOptions(finalOpts...).Syslog)...)
	}
	if hc := cfg.Outputs.HTTP; hc != nil {
		problems = append(problems, validateHTTPConfig(hc, buildOptions(finalOpts...).HTTP)...)
	}
	if len(problems) > 0 {
		return errors.Join(problems...)
	}

	lg := New(finalOpts...) // New 内部：关闭旧默认实例 + 更新 defaultInstance/defaultLeveler

	// 包级 DefaultLogger 与 Fatal 的读取共用 defaultMu（M-5）
	defaultMu.Lock()
	DefaultLogger = lg
	defaultMu.Unlock()
	return nil
}

// validateSyslogConfig 校验 SyslogConfig 节点的合法性，返回问题列表（合并上报用）。
// 地址黑洞基于合并用户 opts 后的最终 SyslogSettings（resolved）判定；其余为 Config 字符串校验。
// 合法值边界与 configOptions / newSyslogHandler 的兜底语义一致：
// Network 仅 ""/tcp/udp（大小写不敏感、trim，"" → 默认 tcp）；Facility 空默认 user、
// 非空须为标准名；Timeout 空默认 5s、非空须可解析；Format 复用 ParseFileFormat。
func validateSyslogConfig(sc *SyslogConfig, resolved *SyslogSettings) []error {
	var problems []error

	// 黑洞：合并后地址仍为空（连接懒建，地址不可达不报错，仅校验声明完整性）。
	if resolved == nil || resolved.Address == "" {
		problems = append(problems, errors.New("logger: syslog output requires non-empty address"))
	}

	switch n := strings.ToLower(strings.TrimSpace(sc.Network)); n {
	case "", "tcp", "udp":
	default:
		problems = append(problems, fmt.Errorf("logger: unknown syslog network %q (want \"tcp\" or \"udp\")", sc.Network))
	}

	if sc.Facility != "" {
		if _, err := ParseSyslogFacility(sc.Facility); err != nil {
			problems = append(problems, err)
		}
	}

	if sc.Timeout != "" {
		if _, err := time.ParseDuration(sc.Timeout); err != nil {
			problems = append(problems, fmt.Errorf("logger: invalid syslog timeout %q: %v", sc.Timeout, err))
		}
	}

	if _, err := ParseFileFormat(sc.Format); err != nil {
		problems = append(problems, err)
	}

	return problems
}

// validateHTTPConfig 校验 HTTPConfig 节点的合法性，返回问题列表（合并上报用）。
// URL 黑洞基于合并用户 opts 后的最终 HTTPSettings（resolved）判定；其余为 Config 字符串校验。
// 合法值边界与 configOptions / newHTTPHandler 的兜底语义一致：
// URL 须可 url.Parse 且 scheme 为 http/https（对端 Vector http_server 只收 HTTP 请求，
// 完整端点含路径——服务端 path 默认精确匹配 "/"）；BatchSize 0 = 默认、负数非法；
// Timeout / FlushInterval 空 = 默认、非空须可 time.ParseDuration 解析且非负。
// 端点可达性不校验（连接懒建，首个批次发送时才拨号）。
func validateHTTPConfig(hc *HTTPConfig, resolved *HTTPSettings) []error {
	var problems []error

	// 黑洞：合并后 URL 仍为空。
	if resolved == nil || resolved.URL == "" {
		problems = append(problems, errors.New("logger: http output requires non-empty url"))
	}

	if hc.URL != "" {
		u, err := url.Parse(hc.URL)
		if err != nil {
			problems = append(problems, fmt.Errorf("logger: invalid http url %q: %v", hc.URL, err))
		} else if u.Scheme != "http" && u.Scheme != "https" {
			problems = append(problems, fmt.Errorf("logger: unsupported http url scheme %q in %q (want \"http\" or \"https\")", u.Scheme, hc.URL))
		}
	}

	if hc.BatchSize < 0 {
		problems = append(problems, fmt.Errorf("logger: invalid http batch_size %d (want 0 for default or positive)", hc.BatchSize))
	}

	problems = append(problems, validateHTTPDuration("http timeout", hc.Timeout, defaultHTTPTimeout)...)
	problems = append(problems, validateHTTPDuration("http flush_interval", hc.FlushInterval, defaultHTTPFlushInterval)...)

	return problems
}

// validateHTTPDuration 校验 http 后端的时长字符串字段：空 = 用默认（不报错）、
// 非空须可 time.ParseDuration 解析且结果非负（负值等价非法参数）。
func validateHTTPDuration(field, s string, def time.Duration) []error {
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return []error{fmt.Errorf("logger: invalid %s %q: %v", field, s, err)}
	}
	if d < 0 {
		return []error{fmt.Errorf("logger: invalid %s %q (want non-negative, %s used when empty)", field, s, def)}
	}
	return nil
}

// configOptions 把 Config 映射为打底的 base Option 列表（Init 的纯映射层，包内 unexported、可单测）。
// 与 Init 解耦，是为了对「ConsoleConfig.NoColor 两态 → ConsoleSettings.Color 三态」等映射做直接断言：非 TTY
// 测试环境端到端无法区分「强制关色」与「自动关色」（输出都无 ANSI），故在映射层锁死该分支。
// 仅负责映射，不做 sink 合法性 / 格式错误校验（那部分由 Init 合并用户 opts 后统一处理）。
func configOptions(cfg Config) []Option {
	// 级别：配置文件优先，环境变量兜底
	level := ParseLevel(cfg.Level)
	if cfg.Level == "" {
		level = LevelFromEnv()
	}

	base := []Option{WithLevel(level)}
	if cfg.Service != "" {
		base = append(base, WithService(cfg.Service))
	}
	if cfg.Env != "" {
		base = append(base, WithEnv(cfg.Env))
	}

	// 控制台后端：节点存在即启用；颜色两态映射——NoColor=true → 追加
	// WithConsoleColor(false) 强制关色；NoColor=false（默认）→ 不追加子选项，
	// WithConsole() 走自动判定（NO_COLOR + TTY）。
	if cfg.Outputs.Console != nil {
		var consoleOpts []ConsoleOption
		if cfg.Outputs.Console.NoColor {
			consoleOpts = append(consoleOpts, WithConsoleColor(false))
		}
		base = append(base, WithConsole(consoleOpts...))
	}

	// 文件后端：节点存在即启用。格式解析字符串为枚举（解析错误由 Init 统一上报，
	// 此处忽略：非法值回退 FormatJSON 只影响随后会被黑洞/格式校验拒绝的路径）；
	// 轮换参数 + 格式统一在此构造。Layout 非空时追加 WithDateRotate → 启用按日期轮换；
	// 空则维持 lumberjack 按大小轮换。
	if cfg.Outputs.File != nil {
		fc := cfg.Outputs.File
		fileFormat, _ := ParseFileFormat(fc.Format)
		fopts := []FileOption{
			WithMaxSize(fc.MaxSize), WithMaxAge(fc.MaxAge),
			WithMaxBackups(fc.MaxBackups), WithCompress(fc.Compress),
			WithFormat(fileFormat),
		}
		if fc.Layout != "" {
			fopts = append(fopts, WithDateRotate(fc.Layout))
		}
		base = append(base, WithFile(fc.Filename, fopts...))
	}

	// syslog 后端：节点存在即启用。格式复用 ParseFileFormat（非法值由 Init 统一上报，
	// 此处忽略）；Timeout 经 time.ParseDuration 解析（非法/空由 Init 上报，此处 0 → handler 兜底默认）。
	// Network/Facility 非法值同样由 Init 拦截，此处原样传入映射为 Option。
	if sc := cfg.Outputs.Syslog; sc != nil {
		syslogFormat, _ := ParseFileFormat(sc.Format)
		sopts := []SyslogOption{
			WithSyslogNetwork(sc.Network),
			WithSyslogTag(sc.Tag),
			WithSyslogHostname(sc.Hostname),
			WithSyslogFacility(sc.Facility),
			WithSyslogFormat(syslogFormat),
		}
		if d, err := time.ParseDuration(sc.Timeout); err == nil {
			sopts = append(sopts, WithSyslogTimeout(d))
		}
		base = append(base, WithSyslog(sc.Address, sopts...))
	}

	// http 后端：节点存在即启用。Timeout/FlushInterval 经 time.ParseDuration 解析
	// （非法值由 Init 统一上报，此处跳过不注入 → WithHTTP 的默认值生效）；
	// BatchSize 仅 >0 时注入（0 = 保持 WithHTTP 默认 100，负值由 Init 拦截）；Headers/Gzip 直接映射。
	if hc := cfg.Outputs.HTTP; hc != nil {
		hopts := []HTTPOption{
			WithHTTPHeaders(hc.Headers),
			WithHTTPGzip(hc.Gzip),
		}
		if hc.BatchSize > 0 {
			hopts = append(hopts, WithHTTPBatchSize(hc.BatchSize))
		}
		if d, err := time.ParseDuration(hc.Timeout); err == nil {
			hopts = append(hopts, WithHTTPTimeout(d))
		}
		if d, err := time.ParseDuration(hc.FlushInterval); err == nil {
			hopts = append(hopts, WithHTTPFlushInterval(d))
		}
		base = append(base, WithHTTP(hc.URL, hopts...))
	}

	if cfg.Source {
		base = append(base, WithSource(true))
	}
	if cfg.Async {
		base = append(base, WithAsync(cfg.QueueSize))
	}

	// 敏感打码（横切能力，console/file 双端生效）：零值=不注入，保持现状。
	// keys 非空 → WithSensitiveKeys（追加词，与内置词集合并）；mask 非空 → WithSensitiveMask。
	if len(cfg.Sensitive.Keys) > 0 {
		base = append(base, WithSensitiveKeys(cfg.Sensitive.Keys...))
	}
	if cfg.Sensitive.Mask != "" {
		base = append(base, WithSensitiveMask(cfg.Sensitive.Mask))
	}

	return base
}
