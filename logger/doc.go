// Package logger 提供基于标准库 log/slog 的日志实现，零包装直出 *slog.Logger。
//
// 设计原则：应用只在初始化时 import 本包，拿到 *slog.Logger 后，打日志完全
// 使用 log/slog 原生 API，不引入任何自有日志类型：
//
//	l := logger.New(
//	    logger.WithService("opencode-api"),
//	    logger.WithEnv("prod"),
//	)
//	slog.Info("service started", "port", 8080) // New 内部已 slog.SetDefault，包级函数开箱即用
//
// # 全局字段
//
// WithService / WithEnv 在 New 时注入 service / env 属性，随每条日志输出。
//
// 在自研 console / fileText（FormatText）handler 中，service / env 与下方 trace_id / req_id
// 一并前置到 msg 之前（固定次序 service → env → trace_id → req_id），并按 record 优先去重；
// JSON handler（FormatJSON）走标准语义，不套用该布局。详见 README「身份与链路字段前置」。
//
// # 链路追踪
//
// handler 链内置 TraceHandler：所有 *Context 方法（InfoContext 等）自动从
// context 中提取非空 trace_id / req_id 附加为日志属性。中间件用 WithTraceID /
// WithReqID 注入即可，业务代码无需手写：
//
//	ctx := logger.WithTraceID(r.Context(), traceID)
//	slog.InfoContext(ctx, "fetching user", slog.Int64("user_id", id))
//
// 未注入时不产生这两个属性，无副作用。
//
// # 输出 sink（按存在性装配）
//
// 控制台与文件是两路独立 sink，各自开关：
//   - WithConsole(opts...) 启用彩色控制台；子选项 WithConsoleWriter(w) 指定 writer
//     （缺省 os.Stdout）、WithConsoleColor(b) 控制颜色（缺省自动：NO_COLOR + 是否 TTY）。
//   - WithFile(path, opts...) 启用文件 sink（lumberjack 按大小 / 按日期轮换）；
//     子选项 WithFormat(logger.FormatText) 切换自研排序 text handler（默认 FormatJSON）。
//
// 两者皆未声明时兜底一个 stdout 控制台，保证 logger.New() 零配置开箱即用、包级日志不静默；
// 仅声明 WithFile 则纯文件输出、不写 stdout；WithConsole + WithFile 即双端输出。
//
// 配置驱动面（Init/Config）：Config.Color 为 *bool 三态，映射到 WithConsoleColor——
// nil=自动判定（NO_COLOR + TTY）、true=强制开色、false=强制关；DefaultConfig 默认 true。
// Config.Layout 非空→WithDateRotate 启用按日期轮换（空则维持 lumberjack 按大小轮换，仅 file/both 消费）；
// Config.Sensitive_Keys / Sensitive_Mask 非空→WithSensitiveKeys / WithSensitiveMask（横切打码，console/file
// 双端生效）；三者零值均不注入对应 Option，DefaultConfig 保持默认行为不变。
//
// # 其余可选能力
//
// 以下能力均为 slog.Handler 装饰器，按需在 New 时启用：异步写入（WithAsync）、
// 敏感信息打码（WithSensitiveKeys）、日志采样（WithSampling）、错误堆栈
// （WithStackTrace，配合 Err / Wrap）、动态调级（WithLeveler 或包级 SetLevel）。
// 可选项未启用时不参与 handler 链。
//
// # 生命周期
//
// 进程退出前调用 Close（flush 异步队列、关闭文件句柄）；Close 返回错误表示
// 异步队列未在超时内排空（存在残余写入与句柄），此时不应再复用相关实例。
// Stats 观测异步丢弃数；Fatal / Fatalf 记录后 flush 并调用 ExitFunc(1)。
// 连续 New / Init 替换默认实例时会先关闭旧默认实例，避免队列与句柄累积。
//
// 引用失效契约：每次 New / Init 之后，先前捕获的一切 *slog.Logger 引用
// （含 logger.DefaultLogger 的旧值）指向已关闭实例——异步链静默丢日志、
// 文件链借写时重开产生残余句柄。长生命周期组件不应跨重建持有引用，
// 须在重建后重新获取 logger.DefaultLogger 或重新构造。
//
// 完整用法与迁移指引见包目录下 README.md。
package logger
