package logger

// Config 日志配置（对齐 aide/internal/log 的 Config，支持 yaml/mapstructure 反序列化）
type Config struct {
	// Level 日志级别：trace, debug, info, warn, error, fatal
	Level string `yaml:"level" mapstructure:"level"`

	// Service 服务名（非空时注入为 service 日志属性）
	Service string `yaml:"service" mapstructure:"service"`

	// Env 运行环境标识（非空时注入为 env 日志属性）
	Env string `yaml:"env" mapstructure:"env"`

	// Output 输出目标：console, file, both（Init 按 sink 存在性映射为分组 Option：
	// console→WithConsole、file→WithFile、both→两者；空值按 console 默认）
	Output string `yaml:"output" mapstructure:"output"`

	// NoColor 禁用控制台颜色（负向命名，零值安全）：false（默认）= 不禁用 → 自动判定
	// （终端支持 ANSI 即应用、跟随 NO_COLOR 环境变量、非 TTY 不涂色）；true = 强制关闭颜色。
	// Config 层不提供「强制开色」——该能力留在 Option 精调层 WithConsoleColor(true)。
	// 仅作用于控制台 sink（Output 为 console/both）；file 输出不受影响。DefaultConfig 零值 false（自动）。
	NoColor bool `yaml:"no_color" mapstructure:"no_color"`

	// File 日志文件路径（当 Output 为 file 或 both 时有效）
	File string `yaml:"file" mapstructure:"file"`

	// MaxSize 单个日志文件最大大小（MB）
	MaxSize int `yaml:"max_size" mapstructure:"max_size"`

	// MaxAge 日志文件最大保留天数
	MaxAge int `yaml:"max_age" mapstructure:"max_age"`

	// MaxBackups 保留的旧日志文件最大数量
	MaxBackups int `yaml:"max_backups" mapstructure:"max_backups"`

	// Compress 是否压缩旧日志文件
	Compress bool `yaml:"compress" mapstructure:"compress"`

	// Layout 文件按日期轮换的布局串（如 "2006-01-02"）。非空 → 启用 rotate 日期轮换
	// （映射既有 FileOption WithDateRotate(layout)）；空 → 维持 lumberjack 按大小轮换。
	// 仅 file/both 消费。DefaultConfig 保持空串（默认行为不变）。
	Layout string `yaml:"layout" mapstructure:"layout"`

	// Async 是否启用异步写入
	Async bool `yaml:"async" mapstructure:"async"`

	// QueueSize 异步队列大小（默认 10240，与 async.go 引擎默认一致）
	QueueSize int `yaml:"queue_size" mapstructure:"queue_size"`

	// Source 是否记录调用者源码位置（文件名:行号）
	Source bool `yaml:"source" mapstructure:"source"`

	// FileFormat 文件输出格式（yaml 字符串，对外契约不变）："json"(默认)/"text"，仅作用于文件
	// handler；Init 经 ParseFileFormat 转为 FileFormat 枚举（大小写不敏感、trim，非法非空值报错）
	FileFormat string `yaml:"file_format" mapstructure:"file_format"`

	// Sensitive_Keys 追加的敏感字段 key（子串匹配、大小写不敏感，与内置词集合并——
	// 与 Option WithSensitiveKeys 语义一致）。非空 → 注入 WithSensitiveKeys(...)；
	// nil/空 → 不注入该 Option（保持现状）。横切能力：console/file 双端都打码。
	Sensitive_Keys []string `yaml:"sensitive_keys" mapstructure:"sensitive_keys"`

	// Sensitive_Mask 自定义敏感掩码。非空 → 注入 WithSensitiveMask(...)；空 → 默认 "******"。
	// 与 Sensitive_Keys 一样遵循零值=不启用注入：仅设本字段不设 keys 时，WithSensitiveMask
	// 会初始化 Sensitive options（打码走内置词集），与直接用 Option 的行为一致。
	Sensitive_Mask string `yaml:"sensitive_mask" mapstructure:"sensitive_mask"`
}

// DefaultConfig 返回默认配置
func DefaultConfig() Config {
	// NoColor 零值 false 即默认（自动判定颜色），无需显式赋值。
	return Config{
		Level:      "info",
		Output:     "console",
		MaxSize:    100,
		MaxAge:     30,
		MaxBackups: 10,
		Compress:   true,
		Async:      false,
		QueueSize:  10240,
		FileFormat: "json", // 文件输出默认 JSON（与既有行为一致）
	}
}
