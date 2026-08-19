package logger

import "io"

// Init 根据 Config 初始化包级默认 logger（对齐 aide 的 Init 语义）。
// 级别解析：配置文件优先，环境变量 LOG_LEVEL 兜底。
// 注意：Init 会替换包级 DefaultLogger，影响后续所有 FromContext/ObtainLogger 的缺省返回。
func Init(cfg Config) error {
	// 级别：配置文件优先，环境变量兜底
	level := ParseLevel(cfg.Level)
	if cfg.Level == "" {
		level = LevelFromEnv()
	}

	var opts []Option
	opts = append(opts, WithLevel(level))

	// 输出目标
	switch cfg.Output {
	case "file":
		// 纯文件输出：控制台丢弃
		opts = append(opts, WithOutput(io.Discard))
		if cfg.File != "" {
			opts = append(opts, WithFile(cfg.File,
				WithMaxSize(cfg.MaxSize), WithMaxAge(cfg.MaxAge),
				WithMaxBackups(cfg.MaxBackups), WithCompress(cfg.Compress)))
		}
	case "both":
		if cfg.File != "" {
			opts = append(opts, WithFile(cfg.File,
				WithMaxSize(cfg.MaxSize), WithMaxAge(cfg.MaxAge),
				WithMaxBackups(cfg.MaxBackups), WithCompress(cfg.Compress)))
		}
	default: // console
	}

	if cfg.Source {
		opts = append(opts, WithSource(true))
	}
	if cfg.Async {
		opts = append(opts, WithAsync(cfg.QueueSize))
	}

	DefaultLogger = New(opts...)
	return nil
}
