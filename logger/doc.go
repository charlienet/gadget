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
// # 可选能力
//
// 以下能力均为 slog.Handler 装饰器，按需在 New 时启用：
// 文件输出与轮换（WithFile，lumberjack 按大小 / 按日期轮换）、异步写入
// （WithAsync）、敏感信息打码（WithSensitiveKeys）、日志采样（WithSampling）、
// 错误堆栈（WithStackTrace，配合 Err / Wrap）、动态调级（WithLeveler 或包级
// SetLevel）。可选项未启用时不参与 handler 链。
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
