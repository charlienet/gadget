package logger

// Config 日志配置（对齐 aide/internal/log 的 Config，支持 yaml/mapstructure 反序列化）
type Config struct {
	// Level 日志级别：trace, debug, info, warn, error, fatal
	Level string `yaml:"level" mapstructure:"level"`

	// Output 输出目标：console, file, both
	Output string `yaml:"output" mapstructure:"output"`

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

	// Async 是否启用异步写入
	Async bool `yaml:"async" mapstructure:"async"`

	// QueueSize 异步队列大小（默认 10000）
	QueueSize int `yaml:"queue_size" mapstructure:"queue_size"`

	// Source 是否记录调用者源码位置（文件名:行号）
	Source bool `yaml:"source" mapstructure:"source"`
}

// DefaultConfig 返回默认配置
func DefaultConfig() Config {
	return Config{
		Level:      "info",
		Output:     "console",
		MaxSize:    100,
		MaxAge:     30,
		MaxBackups: 10,
		Compress:   true,
		Async:      false,
		QueueSize:  10000,
	}
}
