# logger

基于标准库 `log/slog` 的日志包，**零包装直出 `*slog.Logger`**。

设计原则：应用只在**初始化时** import 本包，拿到 `*slog.Logger` 后，打日志完全使用 `log/slog` 原生 API（`slog.Info(...)` / `l.InfoContext(ctx, ...)`），不引入任何自有日志类型。

- **官方标准**：无自定义 Logger 接口，`New` 直接返回 `*slog.Logger` 并接入 `slog.SetDefault`
- **开箱即用**：统一初始化入口，`service`/`env` 全局字段自动注入
- **链路追踪**：`*Context` 方法自动从 `context.Context` 提取 `trace_id`/`req_id` 注入日志属性
- **日志切割**：lumberjack 按大小轮换 / 自研按日期轮换（+gzip+清理）
- **可选装饰器**：异步写入、敏感信息打码、日志采样、错误堆栈、动态调级（全部基于 `slog.Handler`，按需启用）
- **双端输出**：控制台彩色文本，文件 JSON

依赖：仅 `gopkg.in/natefinch/lumberjack.v2`。

## 快速开始

```go
import (
    "log/slog"

    "github.com/charlienet/gadget/logger"
)

func main() {
    // 包加载时 logger.DefaultLogger 已就绪并完成 slog.SetDefault，
    // 零配置即可用 slog 包级函数（stdout 彩色输出、Info 级别）
    slog.Info("service started", "port", 8080)
    slog.Error("db miss", slog.String("user_id", "u1001"))
}
```

## 初始化

### Option 模式

```go
l := logger.New(
    logger.WithService("opencode-api"),   // 全局 service 字段（非空才注入）
    logger.WithEnv("prod"),               // 全局 env 字段
    logger.WithLevel(slog.LevelDebug),    // 静态级别
    logger.WithFile("./logs/app.log",     // 文件输出（JSON 格式）
        logger.WithMaxSize(100),          // MB，默认 100
        logger.WithMaxAge(30),            // 天，默认 30
        logger.WithMaxBackups(10),        // 默认 10
        logger.WithCompress(true),        // 默认 true
        logger.WithDateRotate("2006-01-02")), // 可选：按日期轮换
    logger.WithAsync(),                   // 可选：异步写入（默认队列 10240）
)
// l 是 *slog.Logger，注入业务代码即可

defer logger.Close(0) // 进程退出前 flush 异步队列、关闭文件句柄
```

### Config + Init（配置文件驱动）

```yaml
log:
  level: "info"          # trace | debug | info | warn | error | fatal
  output: "both"         # console | file | both
  file: "./logs/app.log"
  max_size: 100
  max_age: 30
  max_backups: 10
  compress: true
  async: true
  queue_size: 10240
  source: false          # 输出 file:line
  service: "opencode-api"
  env: "prod"
```

```go
var cfg logger.Config
viper.UnmarshalKey("log", &cfg) // 或 yaml.Unmarshal

if err := logger.Init(cfg); err != nil { // 内部自动 slog.SetDefault
    log.Fatalf("init logger: %v", err)
}
```

> `Init` 在 `output: file / both` 但 `file` 为空时返回错误（黑洞配置，日志无处落地），
> 且此时**不**改动 `DefaultLogger`。`Init` / `New` 每次替换默认实例前会关闭（flush + 释放句柄）
> 上一个默认实例，见「设计说明」。

## 链路追踪（trace_id / req_id 自动注入）

handler 链内置 `TraceHandler`：所有 `*Context` 方法（`InfoContext` 等）执行时自动从
ctx 提取非空 `trace_id` / `req_id` 附加为日志属性，**业务代码无需手写**。

```go
// 中间件：把 trace_id / req_id 放进 context（包内类型化 key，无冲突）
func TraceMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        traceID := r.Header.Get("X-Trace-ID")
        if traceID == "" {
            traceID = generateTraceID()
        }
        ctx := logger.WithTraceID(r.Context(), traceID)
        ctx = logger.WithReqID(ctx, generateReqID())
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}

// 业务代码：未注入 ctx 时不含这两个属性，注入后自动带出
func (s *UserService) GetUser(ctx context.Context, userID int64) {
    slog.InfoContext(ctx, "fetching user", slog.Int64("user_id", userID))
}

// 需要回读时
id := logger.GetTraceID(ctx)
```

## 依赖注入模式（推荐）

```go
type UserService struct {
    log *slog.Logger
}

func NewUserService(base *slog.Logger) *UserService {
    // With 是不可变派生：返回新 logger，不影响 base 与其他协程
    return &UserService{log: base.With(slog.String("module", "user_service"))}
}

func (s *UserService) GetUser(ctx context.Context, id int64) {
    s.log.InfoContext(ctx, "fetching user", slog.Int64("user_id", id))
}
```

注意区分两类上下文数据的挂载方式：

- **静态字段**（模块名、组件名）：初始化时用 `With` 绑定
- **请求级字段**（trace_id、req_id）：走 `logger.WithTraceID(ctx, ...)` + `InfoContext(ctx, ...)`，由 handler 自动注入，避免每请求构造临时 logger

## 可选能力

```go
l := logger.New(
    // 错误堆栈：对 logger.Wrap 过的 error 自动附加 "<key>_stack" 属性
    logger.WithStackTrace(true),

    // 敏感信息打码：命中 key 的属性值替换为掩码（Group 递归）
    logger.WithSensitiveKeys("password", "token"), // 子串匹配，与内置词集合并
    logger.WithSensitiveMask("******"),            // 自定义掩码
    // logger.WithSensitiveMatch(func(key string) bool { ... }), // 自定义匹配

    // 日志采样：窗口内前 10 条全留，之后每 100 条留 1 条
    logger.WithSampling(10, 100),

    // 异步写入：非阻塞入队，后台消费
    logger.WithAsync(2048),      // 队列满默认丢弃并计数
    logger.WithAsyncBlocking(),  // 可选：改为阻塞背压，绝不丢日志
)
```

错误堆栈与敏感打码配合使用：

```go
slog.Error("op failed", logger.Err(err)) // logger.Err: error 转 Attr（key 为 "error"）
err = logger.Wrap(err)                   // 记录创建位置调用栈
text := logger.SensitiveString(msg)      // 对消息文本按内置词集打码
```

## 运行时管理与退出

```go
logger.SetLevel(logger.Debug)        // 包级动态调级（对最近一次 New/Init 的实例即时生效）

lvl := logger.NewDynamicLevel(slog.LevelInfo)
l := logger.New(logger.WithLeveler(lvl))
lvl.Set(slog.LevelWarn)              // 自定义 Leveler：级别控制权完全在调用方，
                                     // 包级 SetLevel 对该实例无效

total, dropped := logger.Stats()     // 异步累计统计（total / 因队列满丢弃数）
logger.Fatal("unrecoverable", "component", "broker") // 记录 → flush 异步队列 → ExitFunc(1)
logger.Close(2 * time.Second)        // 进程退出前统一 flush + 关文件
```

`Close` 返回非 nil 错误表示某实例的异步队列**未在 timeout 内排空**（错误信息含残余条数）。
此时该实例的后台消费 goroutine 仍在写入，且文件 writer「写时重开」可能产生永不关闭的残余句柄——
调用方应视该实例存在残余、不再复用其日志能力，只能假定进程即将退出；
需要确定性落盘的场景请加大 timeout 或改用同步 writer（不启用 `WithAsync`）。
另注意：按日期轮换（`WithDateRotate`）的跨日压缩/清理在后台执行，其 `Close` 等待任务收敛
**没有自有超时**——病态文件系统（如 NFS 挂起）下可能超出 `logger.Close(timeout)` 的预算
（见 `rotate.RotateDateWriter.Close` 文档）。

> **引用失效契约**：每次 `New` / `Init` 关闭上一默认实例，先前捕获的一切 `*slog.Logger`
> 引用（含 `logger.DefaultLogger` 旧值）自此陈旧且已关闭——异步链静默丢日志、文件链借
> 「写时重开」复活产生残余句柄。长生命周期组件（内部持有 logger 引用的 cache、监控器等）
> 不应跨重建持有引用：应用运行期重建 logger 后，须重新读取 `logger.DefaultLogger`
> 或重新构造组件。

级别常量：`logger.Trace / Debug / Info / Warn / Error / FatalLevel`（`Level = slog.Level` 别名）。
`logger.ExitFunc` 在测试中可替换，防止 `Fatal` 真实退出进程。

## 从旧版迁移

| 旧 API（已删除） | 新写法 |
| :--- | :--- |
| `l.Info("a", "b")`（拼接为消息） | `slog.Info("a", "b", "c", v)`（msg + 键值对属性） |
| `l.Infof("x=%v", x)` | `slog.Info(fmt.Sprintf("x=%v", x))` |
| `l.WithField(k, v)` / `WithFields(map)` | `l.With(k, v)`（成对参数） |
| `l.SetLevel(lvl)`（实例方法） | `logger.SetLevel(lvl)`（包级）或 `WithLeveler` |
| `l.SetOutput(w)` | 重新 `logger.New(logger.WithOutput(w))` |
| `logger.WithLogger(ctx, l)` / `FromContext` | `logger.WithTraceID(ctx, id)` + `InfoContext(ctx, ...)` |
| 常量 `logger.Fatal` | 常量 `logger.FatalLevel`；函数 `logger.Fatal(msg, args...)` |
| `logger.LogRecorder` / `WithRecorder`（logrus 插件） | 已整体移除，统一走 `slog.Handler` 生态 |

## 设计说明

- handler 链（内 → 外）：`console + file(JSON)` → `StackHandler` → `SensitiveHandler` → `SamplingHandler` → `AsyncHandler` → `TraceHandler`（内置，始终位于最外层）；可选项未启用时不参与链
- `TraceHandler` 置于最外层：`trace_id`/`req_id` 在**调用方 goroutine 内同步提取进 record**后才进入采样 / 异步队列，因此异步队列 entry 无需（也不应）持有请求级 `context.Context`；异步模式下 trace 提取同样生效，且避免了长命队列持有可取消 ctx 的反模式
- 默认输出为 **stdout**；`WithAsync` 队列容量默认 10240（与引擎一致）
- `With` 派生共享底层设施（writer/异步队列/文件句柄），均为并发安全共享；派生实例与父实例的属性互不影响
- **连续 `New` / `Init` 语义**：每次 `New` 在把新实例登记为「默认实例」前，会 `close` 上一个默认实例（flush 其异步队列、关闭文件句柄、从注册表注销），避免队列 / 句柄累积；首次创建（无旧实例）跳过。`SetLevel` / `Fatal` 读取的默认实例引用与 `New` / `Init` 的写入由 `defaultMu` 互斥
