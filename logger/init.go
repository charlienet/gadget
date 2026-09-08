package logger

import (
	"errors"
	"fmt"
	"strings"
)

// ParseFileFormat 解析文件输出格式字符串（Config.FileFormat 的 yaml 面 → FileFormat 枚举）。
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

// Init 根据 Config 初始化包级默认 logger，并接受若干精调 Option（对齐 aide 的 Init 语义）。
// 级别解析：配置文件优先，环境变量 LOG_LEVEL 兜底。
// New 内部已 slog.SetDefault，故 slog 包级函数自动使用本默认 logger；
// Init 会替换包级 DefaultLogger（旧默认实例在 New 内被关闭，见 default.go）。
//
// 合并顺序：Config 打底生成的 base Option 在前、用户 opts 在后，同名 Option 后者胜——
// 用户可用 WithConsole/WithFile/WithSensitiveKeys 等精调或覆盖 Config 的默认映射（Init(cfg) 仍源码兼容）。
//
// sink 映射（Config.output → 分组 Option）：console（及空/未知，按 console 默认）仅启用控制台；
// file 仅启用文件；both 两者都启用。控制台 writer 缺省 os.Stdout。文件输出格式经 ParseFileFormat
// 把 yaml 字符串转为 FileFormat 枚举后由 WithFormat 传入（仅 file/both 消费）。
// 控制台颜色经 Config.NoColor 两态映射（仅 console/both 生效）：false（默认）→ 裸 WithConsole()
// 走自动判定（终端支持 ANSI 即应用、跟随 NO_COLOR、非 TTY 不涂色）；true → WithConsole(WithConsoleColor(false))
// 强制关闭颜色。Config 层不提供强制开色，该能力留在 Option 精调层 WithConsoleColor(true)。
//
// 文件轮换经 Config.Layout 映射（仅 file/both 消费）：非空→WithDateRotate(layout) 启用按日期轮换、
// 空→维持 lumberjack 按大小轮换（DefaultConfig 保持空串）。敏感打码为横切能力（console/file 双端生效）：
// Config.Sensitive_Keys 非空→WithSensitiveKeys（追加词、与内置词集合并）、Config.Sensitive_Mask
// 非空→WithSensitiveMask；二者零值均不注入对应 Option（保持现状）。
//
// 返回错误（均针对文件 sink，合并上报，且不改动 DefaultLogger）：
//   - 黑洞：Config 声明 file/both 但合并后文件路径仍为空（用户可用 WithFile(path) 补路径消除）；
//   - 非法格式：Config.FileFormat 为非空未知值（不静默回退）。
func Init(cfg Config, opts ...Option) error {
	// Config 打底映射抽为纯函数 configOptions（见下），便于映射层单测断言。
	cfgBase := configOptions(cfg)

	// 非法格式校验：仅取 error 值与黑洞校验合并上报（解析值已在 configOptions 内消费）。
	var formatErr error
	if cfg.Output == "file" || cfg.Output == "both" {
		_, formatErr = ParseFileFormat(cfg.FileFormat)
	}

	// 合并：Config 打底在前、用户 opts 在后（后者覆盖同类项）。
	finalOpts := append(cfgBase, opts...)

	// 黑洞 + 非法格式校验后移：基于合并后的最终 sink 结果判定，一并上报，任一失败都不改动 DefaultLogger。
	if cfg.Output == "file" || cfg.Output == "both" {
		var problems []error
		if buildOptions(finalOpts...).File == "" {
			problems = append(problems, fmt.Errorf("logger: output %q requires non-empty file path", cfg.Output))
		}
		if formatErr != nil {
			problems = append(problems, formatErr)
		}
		if len(problems) > 0 {
			return errors.Join(problems...)
		}
	}

	lg := New(finalOpts...) // New 内部：关闭旧默认实例 + 更新 defaultInstance/defaultLeveler

	// 包级 DefaultLogger 与 Fatal 的读取共用 defaultMu（M-5）
	defaultMu.Lock()
	DefaultLogger = lg
	defaultMu.Unlock()
	return nil
}

// configOptions 把 Config 映射为打底的 base Option 列表（Init 的纯映射层，包内 unexported、可单测）。
// 与 Init 解耦，是为了对「NoColor 两态 → ConsoleSettings.Color 三态」等映射做直接断言：非 TTY
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

	// 仅文件 sink（file/both）消费格式：解析 yaml 字符串为枚举；解析错误由 Init 统一上报，
	// 此处忽略（非法值回退 FormatJSON 只影响随后会被黑洞/格式校验拒绝的路径）。
	var fileFormat FileFormat
	if cfg.Output == "file" || cfg.Output == "both" {
		fileFormat, _ = ParseFileFormat(cfg.FileFormat)
	}

	// 文件 sink 子选项：轮换参数 + 格式统一在此构造，供 file/both 分支复用。
	// cfg.Layout 非空时追加 WithDateRotate → 启用按日期轮换；空则维持 lumberjack 按大小轮换。
	fopts := []FileOption{
		WithMaxSize(cfg.MaxSize), WithMaxAge(cfg.MaxAge),
		WithMaxBackups(cfg.MaxBackups), WithCompress(cfg.Compress),
		WithFormat(fileFormat),
	}
	if cfg.Layout != "" {
		fopts = append(fopts, WithDateRotate(cfg.Layout))
	}

	// 控制台颜色两态映射：NoColor=true → 追加 WithConsoleColor(false) 强制关色；
	// NoColor=false（默认）→ 不追加子选项，WithConsole() 走自动判定（NO_COLOR + TTY）。
	var consoleOpts []ConsoleOption
	if cfg.NoColor {
		consoleOpts = append(consoleOpts, WithConsoleColor(false))
	}

	// sink 存在性映射：file 不声明 WithConsole → 纯文件；both 两者都加；其余 → 仅控制台。
	// 控制台 sink 携带颜色子选项（仅当 NoColor=true）；file 分支无控制台，不受 NoColor 影响。
	switch cfg.Output {
	case "file":
		base = append(base, WithFile(cfg.File, fopts...))
	case "both":
		base = append(base,
			WithConsole(consoleOpts...),
			WithFile(cfg.File, fopts...))
	default: // "console" 及空串/未知：仅声明控制台 sink
		base = append(base, WithConsole(consoleOpts...))
	}

	if cfg.Source {
		base = append(base, WithSource(true))
	}
	if cfg.Async {
		base = append(base, WithAsync(cfg.QueueSize))
	}

	// 敏感打码（横切能力，console/file 双端生效）：零值=不注入，保持现状。
	// keys 非空 → WithSensitiveKeys（追加词，与内置词集合并）；mask 非空 → WithSensitiveMask。
	if len(cfg.Sensitive_Keys) > 0 {
		base = append(base, WithSensitiveKeys(cfg.Sensitive_Keys...))
	}
	if cfg.Sensitive_Mask != "" {
		base = append(base, WithSensitiveMask(cfg.Sensitive_Mask))
	}

	return base
}
