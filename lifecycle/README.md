# lifecycle

进程退出时的优雅关闭编排：组件按依赖顺序注册，退出时按注册**逆序**串行关闭，带步超时、总预算、panic 恢复与错误聚合，仅依赖标准库（`go.mod` 无任何 require）。

- **逆序关闭**：注册顺序即依赖顺序——被依赖者先注册，进程入口（HTTP 服务器等最外层）最后注册，因而最先关闭、最底层依赖最后关闭；本包不实现依赖图或拓扑排序，逆序完全由注册顺序决定
- **双触发路径**：`Run(ctx)` 监听 OS 信号（默认 SIGTERM / SIGINT）与 ctx 取消；`Shutdown(ctx)` 显式编程触发。两条路径语义完全一致，可混用
- **关闭只执行一次**：无论哪条路径、哪种触发源启动，后到的 Run/Shutdown 调用（含并发）阻塞等待同一次关闭完成，返回同一份聚合错误
- **故障隔离**：单个组件返回 error、超时或 panic 都不中断其余组件的关闭，错误仅被收集并 `errors.Join` 聚合返回
- **强杀逃生通道（有意设计）**：关闭一经启动立即注销信号句柄，二次同种信号交回 OS 默认动作（终止进程）

## 安装

```bash
go get github.com/charlienet/gadget/lifecycle@v0.1.0
```

## 快速开始

```go
import (
    "context"
    "errors"
    "log/slog"
    "net/http"
    "os"

    "github.com/charlienet/gadget/lifecycle"
)

m := lifecycle.New(
    lifecycle.WithLogger(slog.Default()),
)

// 按依赖顺序注册：被依赖者先注册，入口最后注册
m.Register("db", dbCloser)                          // 实现 lifecycle.Component 的自定义类型
m.Register("cache", lifecycle.Func(func(ctx context.Context) error {
    c.Close()                                       // cache.Close() 无返回值，闭包适配
    return nil
}))
m.Register("http", lifecycle.Func(srv.Shutdown))    // *http.Server 的 Shutdown(ctx) error 直接桥接

// 阻塞直到信号到达 / ctx 取消，执行完关闭后返回聚合错误（全部成功时为 nil）
if err := m.Run(context.Background()); err != nil {
    slog.Error("shutdown errors", "err", err)
    os.Exit(1)
}
```

## 核心 API

### Component 契约

```go
type Component interface {
    Stop(ctx context.Context) error
}
```

实现必须满足三条契约：

1. **幂等**：重复调用 Stop 不产生额外副作用，也不改变已停止的状态；
2. **响应 ctx**：Stop 尊重传入 ctx 的取消/超时，在 `ctx.Done` 时尽快返回（否则该步被记为 `ErrTimeout` 并跳过）；
3. **彻底退出**：Stop 返回时，组件内部启动的所有 goroutine 必须已全部退出。

`Func` 把普通函数适配为 Component：`type Func func(ctx context.Context) error`。

### Manager

| 方法 | 说明 |
|---|---|
| `New(opts...) *Manager` | 构造 Manager（**零值不可用**）；非法 Option 直接 panic（程序期错误） |
| `Register(name, c)` | 注册组件；以下情况 panic：name 为空 / name 重复 / c 为 nil / 关闭已触发后再注册 |
| `Components() []string` | 已注册组件名快照（注册顺序），返回副本可安全修改 |
| `Run(ctx) error` | 注册信号并监听 ctx，阻塞至关闭流程全部执行完毕，返回聚合错误；ctx 取消只是关闭的"原因"，Run **不因 ctx 取消提前返回** |
| `Shutdown(ctx) error` | 启动关闭并返回聚合错误；传入 ctx 仅控制"是否等待完成"——取消时立即返回 `ctx.Err()`，但关闭本身继续后台推进，其它等待者仍收到完整聚合错误 |

多个 Run 并发时，只有**首个 Run** 的信号句柄与 ctx 生效；后续 Run 的 ctx 被取消不会触发任何东西，只继续等待同一次关闭。需要额外触发源请显式调用 `Shutdown`。

### Option

| Option | 默认 | 非法值（`New` 时 panic） |
|---|---|---|
| `WithStepTimeout(d)` | `5s` | `d <= 0` |
| `WithTotalTimeout(d)` | `0`（不设总时限） | `d <= 0` |
| `WithSignals(sigs...)` | `SIGTERM`、`SIGINT` | 零个信号，或含 nil 元素 |
| `WithLogger(l)` | 不输出 | `l == nil` |

logger 类型为 `*slog.Logger`，关闭过程日志以 Info 级输出。

## 错误分类

三个哨兵错误 + 一个名单类型，聚合错误内各条目按 `lifecycle: <name>: <原始错误>` 包装，`errors.Is` / `errors.As` 全程透传：

| 错误 | 触发时机 |
|---|---|
| `ErrTimeout` | 组件 Stop 未在步超时（`stepCtx`）内返回；残留 goroutine 不会被 kill（Go 无法强杀），流程继续下一步 |
| `ErrPanicked` | 组件 Stop 发生 panic，已被 recover，错误值附带堆栈；配置了 logger 时同步记录 |
| `ErrBudgetExhausted` | 关闭总预算（`WithTotalTimeout`）耗尽 |
| `*SkippedError` | 因预算耗尽**根本没被调用 Stop** 的组件名单（`Names []string`），`Unwrap` 到 `ErrBudgetExhausted` |

```go
err := m.Shutdown(ctx)
var se *lifecycle.SkippedError
switch {
case errors.Is(err, lifecycle.ErrTimeout):
    // 某组件关闭超时
case errors.Is(err, lifecycle.ErrPanicked):
    // 某组件关闭时 panic
case errors.As(err, &se):
    // se.Names 中的组件完全未被停止
}
```

## 超时模型

- 每步的 `stepCtx` = 根 ctx 之上再限 `min(剩余总预算, stepTimeout)`；
- 根 ctx 仅在设置 `WithTotalTimeout` 时携带超时（否则是 `context.Background()`，不设总时限）；
- 每步开始前检查总预算：一旦耗尽，剩余组件**全部跳过**并计入一条 `*SkippedError`（不逐个报错，避免文案重复）。

## 级联关闭：组件级事实，不可外推

"某个组件关闭时会不会顺手把它内部依赖也关掉"，是该组件自身的实现事实，只能逐个查证，不能当通则。本仓库当前实情：

- cache 的 `Close` 会级联关闭其内部 localStore / remoteStore；
- redis 的 `GracefulClose` 会级联关闭 `AddPrefix` 派生的全部子连接池；
- 但上述级联仅对实现了 `interface{ Close() }`（无返回值）的内部依赖生效；gadget/store 约定的签名是 `Store.Close() error`，类型断言失败后该依赖被**静默跳过**——cache 并不会替你关掉一个 `store.Store`。

结论：需要被关闭的底层依赖（store、redis 客户端等）**一律独立 `Register`**，不要指望容器组件代为关闭。若容器与内部依赖同时注册形成双路径关闭，本包不检测也不去重，由 Component 幂等契约兜底（实害为零，只是多一次调用）。

## 注册顺序要点

- **logger 自指顺序**：若把日志器本身注册为组件（如异步 logger 退出时需 flush），它必须**最先注册**（因而在关闭流程最后一步被关闭），保证其它组件关闭期间仍有日志可用；反之若最后注册则它最先关闭，其它组件的关闭日志将丢失。
- 关闭启动时对组件表取快照，与并发 Register 线性化：快照之后的注册会被 `Register` panic 拒绝。
