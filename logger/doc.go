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
// 在自研 console handler（文件 text sink FormatText 即其 NoColor 形态，同一实现）中，
// service / env 与下方 trace_id / req_id 一并前置到 msg 之前（固定次序
// service → env → trace_id → req_id），并按 record 优先去重；版式：前置段（time / level /
// 命中字段）与 msg 均为裸值（无 key= 前缀、msg 不加引号；msg 中 \n/\r 以字面量转义
// 保持单行——防按 req_id/trace_id 过滤断行，不加引号，\t 等其余字符原样），
// source= 与其余 attrs 保持 k=v，
// source（若启用）恒在行尾；控制台通道仅在此之上叠加 ANSI 颜色，关闭颜色后两通道字节等同。
// attr 值渲染先解析 slog.LogValuer（对齐标准库）：LogValue() 返回 Group 时按 key.sub=… 递归
// 展开、string 等形态按对应 Kind 处理；未实现 LogValuer 的普通 Any 值仍 encoding.TextMarshaler
// 优先、%+v 兜底。
// JSON handler（FormatJSON）走标准语义，不套用该布局。
// 详见 README「身份与链路字段前置」。
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
// 控制台、文件、syslog 是多路独立 sink，各自开关，多路经同一 MultiHandler 扇出：
//   - WithConsole(opts...) 启用彩色控制台；子选项 WithConsoleWriter(w) 指定 writer
//     （缺省 os.Stdout）、WithConsoleColor(b) 控制颜色（缺省自动：NO_COLOR + 是否 TTY）。
//   - WithFile(path, opts...) 启用文件 sink（lumberjack 按大小 / 按日期轮换）；
//     子选项 WithFormat(logger.FormatText) 切换 text 输出（console 渲染器 NoColor 形态，默认 FormatJSON）。
//   - WithSyslog(address, opts...) 启用 syslog sink：逐条发送 RFC5424 报文、\n 分帧，适配对端
//     Vector syslog source（自动识别 5424）。连接懒建 / 断线后台重连，失败期间该条丢弃 + stderr
//     限流告警，绝不阻塞调用方超过 Timeout。子选项 WithSyslogNetwork/Tag/Hostname/Facility/Format/Timeout。
//   - WithHTTP(url, opts...) 启用 http sink：以 NDJSON 批量 POST 到远端 HTTP 收集端（适配对端
//     Vector sources.http_server，codec=json 逐行解码，每行含末尾换行）。攒满 BatchSize 或 FlushInterval 到点即换出，
//     由独立发送 worker 做网络 IO（日志调用方只缓冲，永不受网络耗时阻塞）；网络错误 / 5xx / 408 / 429
//     整批指数退避重试（含首次共 3 次尝试），其余 4xx（对端任一坏帧即整批 400）为确定性失败**不重试**；
//     最终失败丢弃该批 + stderr 限流告警（同原因每 5s ≤1 条）。语义为
//     at-least-once，重试可致对端重复。Close / Fatal 退出前会收尾投递残余缓冲批（限时、不重试）。
//     子选项 WithHTTPHeaders/BatchSize/FlushInterval/Timeout/Gzip。
//
// 均未声明时兜底一个 stdout 控制台，保证 logger.New() 零配置开箱即用、包级日志不静默；
// 仅声明非 console sink（WithFile / WithSyslog / WithHTTP）则不写 stdout；WithConsole + 其它即多端输出。
//
// 配置驱动面（Init/Config）：后端按 Config.Outputs 节点存在性启用——Console 节点非 nil→WithConsole、
// File 节点非 nil→WithFile、Syslog 节点非 nil→WithSyslog、HTTP 节点非 nil→WithHTTP，各节点皆 nil 由 New 兜底 stdout 控制台
// （DefaultConfig 为纯 console：Console=&ConsoleConfig{}、File/Syslog/HTTP=nil）。ConsoleConfig.NoColor 为
// 负向命名 bool（零值安全），映射到 WithConsoleColor——false（默认/未配置）=自动判定（终端支持 ANSI
// 且为 TTY 且无 NO_COLOR 才涂色）、true=强制关色；Config 层不提供强制开色，该能力留在 Option 精调层
// WithConsoleColor(true)。
// FileConfig.Layout 非空→WithDateRotate 启用按日期轮换（空则维持 lumberjack 按大小轮换，仅 File 节点消费）；
// SyslogConfig 的 Address 非空、Network/Facility/Timeout/Format 合法性在 Init 合并后校验（不可达地址
// 不在 Init 报错，连接为懒建）；HTTPConfig 的 URL 非空且 scheme 必须 http/https、BatchSize 非负、
// Timeout / FlushInterval 非空时可解析且非负，同样在 Init 合并后校验（端点不可达不在 Init 报错，
// 首个批次发送时才拨号）；Config.Sensitive.Keys / Sensitive.Mask 非空→WithSensitiveKeys / WithSensitiveMask
// （横切打码，各 sink 生效）；零值均不注入对应 Option，DefaultConfig 保持默认行为不变。
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
// 进程退出前调用 Close（flush 异步队列、关闭文件句柄、关闭 syslog 连接、停 http 发送 worker 并最后投递
// 一次残余批）；Close 返回错误表示异步队列未在超时内排空（存在残余写入与句柄），此时不应再复用相关实例。
// syslog Close 后不再重连写出；http Close 后 Handle 静默丢弃（其内部等待上界为 2×Timeout，超时残留按
// 丢弃 + 限流告警处理）。Stats 观测异步丢弃数（不含 syslog / http 发送失败丢弃，后者以 stderr 限流告警暴露）；
// Fatal / Fatalf 记录后对默认实例执行完整 sink 释放链（异步 flush + 文件/syslog/http 关闭，
// 限时 2s，http 缓冲批亦尽力送出）再调用 ExitFunc(1)。
// 连续 New / Init 替换默认实例时会先关闭旧默认实例，避免队列与句柄累积。
//
// 引用失效契约：每次 New / Init 之后，先前捕获的一切 *slog.Logger 引用
// （含 logger.DefaultLogger 的旧值）指向已关闭实例——异步链静默丢日志、
// 文件链借写时重开产生残余句柄。长生命周期组件不应跨重建持有引用，
// 须在重建后重新获取 logger.DefaultLogger 或重新构造。
//
// 完整用法与迁移指引见包目录下 README.md。
package logger
