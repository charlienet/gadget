package logger

// Config 日志配置。导出字段携带 yaml / json / mapstructure tag（键名统一 snake_case，
// 层级嵌套由子结构自然承载），便于应用端直接嵌入（如 Logger: logger.Config）后用
// 对应解码器（viper / yaml.Unmarshal / json.Unmarshal / mapstructure 等）装配；
// 解析方式仍由应用端负责，logger 库本身不含解析代码，Init 只接收装配好的结构体。
// 后端按 Outputs 分组声明——某节点非 nil = 启用该后端，nil = 不启用（不存在 Output
// 字符串开关）。
type Config struct {
	// Level 日志级别：trace, debug, info, warn, error, fatal
	Level string `yaml:"level" json:"level" mapstructure:"level"`

	// Service 服务名（非空时注入为 service 日志属性）
	Service string `yaml:"service" json:"service" mapstructure:"service"`

	// Env 运行环境标识（非空时注入为 env 日志属性）
	Env string `yaml:"env" json:"env" mapstructure:"env"`

	// Source 是否记录调用者源码位置（文件名:行号）
	Source bool `yaml:"source" json:"source" mapstructure:"source"`

	// Async 是否启用异步写入
	Async bool `yaml:"async" json:"async" mapstructure:"async"`

	// QueueSize 异步队列大小（默认 10240，与 async.go 引擎默认一致）
	QueueSize int `yaml:"queue_size" json:"queue_size" mapstructure:"queue_size"`

	// Sensitive 敏感信息打码配置（横切能力，console/file 双端生效）
	Sensitive SensitiveConfig `yaml:"sensitive" json:"sensitive" mapstructure:"sensitive"`

	// Outputs 输出后端分组（节点存在性 = 启用开关）
	Outputs OutputsConfig `yaml:"outputs" json:"outputs" mapstructure:"outputs"`
}

// SensitiveConfig 敏感打码分组（映射 Option：WithSensitiveKeys / WithSensitiveMask）。
type SensitiveConfig struct {
	// Keys 追加的敏感字段 key（子串匹配、大小写不敏感，与内置词集合并——
	// 与 Option WithSensitiveKeys 语义一致）。非空 → 注入 WithSensitiveKeys(...)；
	// nil/空 → 不注入该 Option（保持现状）。
	Keys []string `yaml:"keys" json:"keys" mapstructure:"keys"`

	// Mask 自定义敏感掩码。非空 → 注入 WithSensitiveMask(...)；空 → 默认 "******"。
	// 与 Keys 一样遵循零值=不启用注入：仅设 Mask 不设 Keys 时，WithSensitiveMask
	// 会初始化 Sensitive options（打码走内置词集），与直接用 Option 的行为一致。
	Mask string `yaml:"mask" json:"mask" mapstructure:"mask"`
}

// OutputsConfig 后端分组：某节点非 nil = 启用该后端；nil = 不启用。不再有 Output 字符串。
// 各 sink 节点皆 nil 时由 New 的兜底装配保证输出到 stdout 控制台（包级日志不静默）；
// 显式声明了任一非 console sink（file / syslog / http）而未声明 console 时不兜底 stdout。
// 扩展点（console / file / syslog / http 已全部落地）：再加新后端时照此模式在此加指针字段
// （如 Foo *FooConfig）+ configOptions 里加一条「节点存在性 → 对应分组 Option」映射分支，
// 并把新 sink 并入 default.go 的 hasNonConsoleSink 表达式，勿在 shouldEnableConsole 内加分支。
type OutputsConfig struct {
	// Console 控制台后端（nil = 不启用）。映射 Option：WithConsole(ConsoleOption...)。
	Console *ConsoleConfig `yaml:"console" json:"console" mapstructure:"console"`

	// File 文件后端（nil = 不启用）。映射 Option：WithFile(Filename, FileOption...)。
	File *FileConfig `yaml:"file" json:"file" mapstructure:"file"`

	// Syslog 后端（nil = 不启用）。映射 Option：WithSyslog(Address, SyslogOption...)。
	// 向远端 syslog 服务（对端 Vector 0.58 syslog source）发送 RFC5424 报文，newline 分帧。
	Syslog *SyslogConfig `yaml:"syslog" json:"syslog" mapstructure:"syslog"`

	// HTTP 后端（nil = 不启用）。映射 Option：WithHTTP(URL, HTTPOption...)。
	// 向远端 HTTP 收集端（对端 Vector 0.58 sources.http_server）批量 POST NDJSON。
	HTTP *HTTPConfig `yaml:"http" json:"http" mapstructure:"http"`
}

// ConsoleConfig 控制台后端参数。
type ConsoleConfig struct {
	// NoColor 禁用控制台颜色（负向命名，零值安全），语义与旧平铺字段完全一致：
	// false（默认）= 自动判定（终端支持 ANSI + 是 TTY + 无 NO_COLOR 环境变量才涂色）；
	// true = 强制关闭颜色。Config 层不提供「强制开色」——该能力留在 Option 精调层
	// WithConsoleColor(true)。
	// 映射：nil Console 节点不启用控制台；非 nil 且 NoColor=false → WithConsole()；
	// NoColor=true → WithConsole(WithConsoleColor(false))。
	NoColor bool `yaml:"no_color" json:"no_color" mapstructure:"no_color"`
}

// FileConfig 文件后端参数（映射 Option：WithFile(path, FileOption...)）。
type FileConfig struct {
	// Filename 日志文件路径；File 节点非 nil 时必须非空（Init 合并用户 opts 后做黑洞校验）
	Filename string `yaml:"filename" json:"filename" mapstructure:"filename"`

	// Format 文件输出格式："json"(默认)/"text"，经 ParseFileFormat 转枚举
	// （大小写不敏感、trim，非法非空值报错）→ WithFormat
	Format string `yaml:"format" json:"format" mapstructure:"format"`

	// MaxSize 单个日志文件最大大小（MB）→ WithMaxSize
	MaxSize int `yaml:"max_size" json:"max_size" mapstructure:"max_size"`

	// MaxAge 日志文件最大保留天数 → WithMaxAge
	MaxAge int `yaml:"max_age" json:"max_age" mapstructure:"max_age"`

	// MaxBackups 保留的旧日志文件最大数量 → WithMaxBackups
	MaxBackups int `yaml:"max_backups" json:"max_backups" mapstructure:"max_backups"`

	// Compress 是否压缩旧日志文件 → WithCompress
	Compress bool `yaml:"compress" json:"compress" mapstructure:"compress"`

	// Layout 文件按日期轮换的布局串（如 "2006-01-02"）。非空 → WithDateRotate(layout)
	// 启用按日期轮换；空 → 维持 lumberjack 按大小轮换。
	Layout string `yaml:"layout" json:"layout" mapstructure:"layout"`
}

// SyslogConfig syslog 后端参数（映射 Option：WithSyslog(address, SyslogOption...)）。
// 报文为 RFC5424 单行 + 尾 \n（对端 Vector newline 分帧）。连接懒建 / 后台重连，
// 地址不可达不在 Init 报错（与 file 的 lumberjack 惰性 IO 语义一致）。
type SyslogConfig struct {
	// Network 传输协议："tcp"(默认)/"udp"，其它值 Init 报错 → WithSyslogNetwork
	Network string `yaml:"network" json:"network" mapstructure:"network"`

	// Address 远端地址（如 "127.0.0.1:6514"）；Syslog 节点非 nil 时必填（Init 校验）→ WithSyslog 首参
	Address string `yaml:"address" json:"address" mapstructure:"address"`

	// Tag RFC5424 APP-NAME；空则回退 Config.Service，再空回退 "gadget" → WithSyslogTag
	Tag string `yaml:"tag" json:"tag" mapstructure:"tag"`

	// Hostname RFC5424 HOSTNAME；空则 os.Hostname() → WithSyslogHostname
	Hostname string `yaml:"hostname" json:"hostname" mapstructure:"hostname"`

	// Facility syslog 设施名（空默认 "user"(1)；支持 kern/user/daemon/local0-local7 等标准名，
	// 非法名 Init 报错）→ WithSyslogFacility
	Facility string `yaml:"facility" json:"facility" mapstructure:"facility"`

	// Format MSG 体渲染格式："json"(默认)/"text"，复用 ParseFileFormat 转枚举 → WithSyslogFormat
	Format string `yaml:"format" json:"format" mapstructure:"format"`

	// Timeout 单次 dial/write 超时（time.ParseDuration，如 "5s"）；空默认 "5s"，
	// 非法值 Init 报错 → WithSyslogTimeout
	Timeout string `yaml:"timeout" json:"timeout" mapstructure:"timeout"`
}

// HTTPConfig http 后端参数（映射 Option：WithHTTP(url, HTTPOption...)）。
// 以 NDJSON 批量 POST 到远端 HTTP 收集端（对端 Vector 0.58 sources.http_server，
// codec = json，逐行解码；每帧含自身换行、批体亦以换行收尾）。连接懒建（首个批次发送时才拨号），
// 端点不可达不在 Init 报错（与 file 的 lumberjack 惰性 IO、syslog 的懒 dial 语义一致）。
// 投递语义：网络错误 / 5xx / 408 / 429 整批退避重试（共 3 次尝试），其余 4xx（对端任一坏帧
// 即整批 400）不重试直接丢弃；失败均按条计 dropped + stderr 限流告警。Close 与 Fatal 退出前
// 会收尾投递残余缓冲批（单次快速尝试、限时 Timeout）。
type HTTPConfig struct {
	// URL 完整 POST 端点（含路径，如 "http://127.0.0.1:8080/v1/logs"）；HTTP 节点非 nil
	// 时必填，Init 经 url.Parse 校验且 scheme 必须 http/https → WithHTTP 首参
	URL string `yaml:"url" json:"url" mapstructure:"url"`

	// Headers 附加请求头（认证 token 等由应用端注入，本库不含鉴权语义）；
	// 逐条 Set 到请求，后于内置 Content-Type → 可覆写默认头 → WithHTTPHeaders
	Headers map[string]string `yaml:"headers" json:"headers" mapstructure:"headers"`

	// BatchSize 每次 POST 的最大条数；0 = 默认 100，负数 Init 报错 → WithHTTPBatchSize
	BatchSize int `yaml:"batch_size" json:"batch_size" mapstructure:"batch_size"`

	// FlushInterval 不满批时的强制投递间隔（time.ParseDuration，如 "500ms"）；
	// 空默认 "500ms"，非法/负值 Init 报错 → WithHTTPFlushInterval
	FlushInterval string `yaml:"flush_interval" json:"flush_interval" mapstructure:"flush_interval"`

	// Timeout 整请求超时（含重试的单次尝试），time.ParseDuration 如 "5s"；
	// 空默认 "5s"，非法/负值 Init 报错 → WithHTTPTimeout
	Timeout string `yaml:"timeout" json:"timeout" mapstructure:"timeout"`

	// Gzip 请求体 gzip 压缩（置 Content-Encoding: gzip）→ WithHTTPGzip
	Gzip bool `yaml:"gzip" json:"gzip" mapstructure:"gzip"`
}

// DefaultConfig 返回默认配置：纯 console（等价旧格式仅启用 console 节点），Outputs.File 为 nil。
//
// File 后端默认参数不进入 DefaultConfig（File 节点默认 nil、不启用文件输出）；
// 启用文件时可按如下模板构造，与 Option 层默认一致：
//
//	cfg.Outputs.File = &FileConfig{
//	    MaxSize:    100,
//	    MaxAge:     30,
//	    MaxBackups: 10,
//	    Compress:   true,
//	    Format:     "json", // ParseFileFormat 空串亦回退 FormatJSON
//	}
//
// Console 的 NoColor 零值 false 即默认（自动判定颜色），无需显式赋值。
//
// Syslog / HTTP 后端同理不入 DefaultConfig（节点默认 nil、不启用远端输出）；启用 http 时可按如下
// 模板构造，与 Option 层（WithHTTP）默认一致：
//
//	cfg.Outputs.HTTP = &HTTPConfig{
//	    URL:           "http://127.0.0.1:8686/v1/logs", // 必填，完整 POST 端点
//	    BatchSize:     100,     // 0 亦回退默认 100
//	    FlushInterval: "500ms", // 空亦回退默认 500ms
//	    Timeout:       "5s",    // 空亦回退默认 5s
//	}
func DefaultConfig() Config {
	return Config{
		Level:     "info",
		Async:     false,
		QueueSize: 10240,
		Outputs: OutputsConfig{
			Console: &ConsoleConfig{},
		},
	}
}
