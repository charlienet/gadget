package logger

import (
	"fmt"
	"io"
)

// Init 根据 Config 初始化包级默认 logger（对齐 aide 的 Init 语义）。
// 级别解析：配置文件优先，环境变量 LOG_LEVEL 兜底。
// New 内部已 slog.SetDefault，故 slog 包级函数自动使用本默认 logger；
// Init 会替换包级 DefaultLogger（旧默认实例在 New 内被关闭，见 default.go）。
//
// 返回错误：Output 为 "file"/"both" 但 File 为空（否则日志将无处落地——
// console 被丢弃、文件路径缺失，形成黑洞配置），此时不改动 DefaultLogger。
func Init(cfg Config) error {
	// M-6：黑洞配置校验（在任何全局状态变更前完成，失败即不替换默认 logger）
	switch cfg.Output {
	case "file", "both":
		if cfg.File == "" {
			return fmt.Errorf("logger: output %q requires non-empty file path", cfg.Output)
		}
	}

	// 级别：配置文件优先，环境变量兜底
	level := ParseLevel(cfg.Level)
	if cfg.Level == "" {
		level = LevelFromEnv()
	}

	var opts []Option
	opts = append(opts, WithLevel(level))

	if cfg.Service != "" {
		opts = append(opts, WithService(cfg.Service))
	}
	if cfg.Env != "" {
		opts = append(opts, WithEnv(cfg.Env))
	}

	// 输出目标
	switch cfg.Output {
	case "file":
		// 纯文件输出：控制台丢弃
		opts = append(opts, WithOutput(io.Discard))
		opts = append(opts, WithFile(cfg.File,
			WithMaxSize(cfg.MaxSize), WithMaxAge(cfg.MaxAge),
			WithMaxBackups(cfg.MaxBackups), WithCompress(cfg.Compress)))
	case "both":
		opts = append(opts, WithFile(cfg.File,
			WithMaxSize(cfg.MaxSize), WithMaxAge(cfg.MaxAge),
			WithMaxBackups(cfg.MaxBackups), WithCompress(cfg.Compress)))
	default: // console
	}

	if cfg.Source {
		opts = append(opts, WithSource(true))
	}
	if cfg.Async {
		opts = append(opts, WithAsync(cfg.QueueSize))
	}

	lg := New(opts...) // New 内部：关闭旧默认实例 + 更新 defaultInstance/defaultLeveler

	// 包级 DefaultLogger 与 Fatal 的读取共用 defaultMu（M-5）
	defaultMu.Lock()
	DefaultLogger = lg
	defaultMu.Unlock()
	return nil
}
