# leader

基于 `lock` 的 leader 选举状态机，借鉴 client-go leaderelection 的三参数模型（LeaseDuration / RenewDeadline / RetryPeriod）。

- **只依赖 `lock.Locker` 语义、后端无关**：选举所需的最小能力被收敛为 `Locker` 接口（`TryLock` / `Renew` / `Unlock`，`*lock.Lock` 编译期断言天然满足）；Redis/etcd 等后端的装配发生在业务组合根，本模块不感知后端
- **互斥裁决完全委托给注入的 Locker**：leader 只做选举状态机——竞选轮询、续约预算、让位清理，不自己实现任何锁协议
- **任期（term）机制**：每次成功当选 term 单调递增并作为参数传入 `OnStartedLeading`，供业务侧识别/拒绝旧任期的僵尸回调（fencing 边界见下文，须诚实对待）
- **让位即清理**：五条让位路径共用同一清理序列（isLeader 置 false → 取消业务 ctx → 尽力释放锁 → `OnStoppedLeading` 恰好一次），回调严格成对
- **保守侧设计**：宁可误让位不可双主——续约返回 `(false, nil)` 立即让位，`err != nil` 一律不信任 `ok` 分量

## 安装

```bash
go get github.com/charlienet/gadget/leader@v0.1.0
```

## 快速开始

用 `lock` + `plugins/lock/redis` 的真实装配（与 package doc 的用法代码块一致）：

```go
import (
    "context"
    "errors"
    "log"
    "time"

    goredis "github.com/redis/go-redis/v9"

    "github.com/charlienet/gadget/leader"
    "github.com/charlienet/gadget/lock"
    redislock "github.com/charlienet/gadget/plugins/lock/redis"
)

rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379"})

// 锁的构造（key、TTL、后端、FailPolicy）全部在组合根完成
l := lock.New("svc:leader",
    lock.WithBackend(redislock.New(rdb)), // 组合根选择后端
    lock.WithTTL(15*time.Second),         // 应 == WithLeaseDuration
)

e := leader.New(
    leader.WithLocker(l),
    leader.WithCallbacks(leader.Callbacks{
        OnStartedLeading: func(ctx context.Context, term uint64) { runWork(ctx, term) },
        OnStoppedLeading: func() { cleanup() },
    }),
)

// 一轮完整选举：阻塞竞选 → 当选 → 续约 → 让位返回
if err := e.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
    log.Printf("选举结束: %v", err)
}
```

让位后自动重新竞选：外层 `for` 循环串行调用 `Run` 即可（同一 Elector 支持串行多轮，每轮 term 递增）。

## 核心 API

### Elector

| 方法 | 说明 |
|---|---|
| `New(opts...) *Elector` | 构造 Elector（**零值不可用**）；未注入 Locker、`OnStartedLeading` 为 nil、或参数不满足 `LeaseDuration > RenewDeadline > RetryPeriod > 0` 时 panic（构造期编程错误 fail-fast） |
| `Run(ctx) error` | 执行一轮完整选举：阻塞竞选 → 当选领导 → 让位返回；**不支持并发调用**（第二次并发 Run panic），同一 Elector 可串行多次 Run |
| `IsLeader() bool` | 当前是否处于领导状态（当选确认后置位，让位决策时清除）；可被任意 goroutine 并发调用 |
| `Term() uint64` | 最近一次成功当选的任期号（从未当选为 0）；返回"当前值"，与回调参数的区别见下文 |
| `Identity() string` | 本节点竞选身份（默认 `"hostname-pid"`）；仅用于日志/观测自描述——受 lock 抽象限制（无持有者探查能力），本模块无法感知其他竞选者的身份 |

### Option

| Option | 默认值 | 非法值处理 | 说明 |
|---|---|---|---|
| `WithLocker(l)` | 无（必选） | `nil` 忽略；缺失时 `New` panic | 注入锁能力，生产装配即 `lock.New(key, ...)`；`lock.WithTTL` 应设为与 `WithLeaseDuration` 相同的值（获锁租约由 lock 实例决定、续约租约由 LeaseDuration 决定，两者一致租约语义才连续） |
| `WithIdentity(id)` | `"hostname-pid"` | 空串忽略 | 仅日志/观测自描述 |
| `WithCallbacks(c)` | 零值 | `OnStartedLeading` 为 nil 时 `New` panic | 生命周期回调，见下节 |
| `WithLeaseDuration(d)` | `15s` | `d <= 0` 忽略 | 租约时长：续约时延长至该值，也是本节点失联后他人接管等待上限；须 > RenewDeadline |
| `WithRenewDeadline(d)` | `10s` | `d <= 0` 忽略 | 续约预算：距上次成功续约超过该时长仍未成功，在租约到期前主动让位；须 > RetryPeriod |
| `WithRetryPeriod(d)` | `2s` | `d <= 0` 忽略 | 竞选轮询与续约的基准间隔，实际带 `[1.0, 1.25)` 倍抖动 |

与 retry 包惯例一致：非法 Option 值**静默忽略**（保持默认）；panic 只发生在 `New` 的必选项缺失或参数矛盾时。三个时长默认值与 client-go leaderelection 一致。

### Callbacks

```go
type Callbacks struct {
    OnStartedLeading func(ctx context.Context, term uint64) // 必选
    OnStoppedLeading func()                                 // 可选，nil 跳过
}
```

- `OnStartedLeading` 在当选后以**独立 goroutine** 调用（每轮 Run 至多一次，只在当选确认后触发）；`ctx` 在让位时被取消，`term` 为本轮任期号。**必须响应 ctx 取消并及时返回**——leader 不等待其退出即继续让位流程（与 client-go 一致）
- `OnStoppedLeading` 在每轮成功当选的让位流程末尾**同步**调用，恰好一次，发生在业务 ctx 取消与锁释放（尽力而为）之后、Run 返回之前；未当选过的 Run 不触发——与 `OnStartedLeading` 严格成对
- 所有回调必须快速返回、不得阻塞（长驻业务逻辑在 `OnStartedLeading` 内部自行派生 goroutine）
- **panic 不做 recover，直接穿透**（与 retry.Do 对 fn panic 的约定一致）：`OnStartedLeading` panic 使进程崩溃；`OnStoppedLeading` panic 穿透出 Run 时，stepDown 顺序保证此时锁已释放、业务 ctx 已取消，残渣仅为该回调自身未完成；`running` 与 `IsLeader` 标志均经 defer 复位，再次 Run 后读数不受影响

## 让位路径与 Run 返回值

七条终止路径（源码 `leader.go`，均有测试佐证）：

| 触发 | Run 返回 | 回调 |
|---|---|---|
| 竞选阶段 ctx 取消/超时（未当选） | `ctx.Err()` | 零回调、零 term 消耗 |
| 当选瞬间检出 ctx 已取消 | `ctx.Err()`（先尽力 Unlock） | 零回调、零 term 消耗 |
| 当选后外层 ctx 取消（优雅退出） | `ctx.Err()` | OnStoppedLeading |
| 续约返回 `(false, nil)`（确认锁丢失） | `ErrLeadershipLost`（附 "renew confirmed lock lost"） | 同上 |
| 续约预算耗尽（RenewDeadline 内未成功） | `ErrLeadershipLost`（附最后一次续约错误） | 同上 |
| 后端不支持续约 | `errors.Join(ErrLeadershipLost, lock.ErrRenewUnsupported)`，`errors.Is` 两者同时命中 | 同上 |
| `OnStartedLeading` 自行返回（主动让位） | `nil` | 同上 |

- `ErrLeadershipLost` 是导出哨兵，用 `errors.Is` 判定；竞选阶段 `TryLock` 的 `err`（含 `lock.ErrBackendUnavailable`）**不终止竞选**，静默按 RetryPeriod 轮询直到 ctx 取消
- 多让位信号同时就绪（如 ctx 取消与续约失败同瞬）时 select 随机选取，返回值的分类边界存在二义，但均为合法让位，回调成对不变量不受影响
- 锁释放为尽力而为（`context.WithoutCancel` + 独立 5s 超时），失败不影响返回值——锁最迟在 LeaseDuration 后自然过期（自最后一次成功续约起算）

## 双主窗口：压缩、但不为零

旧 leader 进程冻结/复活场景的双写窗口只能靠租约时序**压缩**，无法根除。上界：**≤ 1.25×RetryPeriod + 一次续约调用耗时**——僵尸 leader 恢复后首次续约返回 `(false, nil)`（确认丢失）即立即让位，不等预算耗尽；而续约节拍本身带 `[1.0, 1.25)` 抖动。此外 `cancelLead` 之后不等待 `OnStartedLeading` 返回即释放锁，业务对取消的响应时延构成理论上的附加窗口，需以 term 贯通写路径缓解。

## fencing 边界（诚实声明）

term 仅在**单进程内**单调递增，只具备进程内的 fencing 意义；**跨进程强 fencing 不提供**（lock 抽象当前无存储侧单调版本号能力）。需要强 fencing 的业务须在存储侧自行校验版本号单调性（如条件写携带 term、仅接受更大版本）。业务侧识别僵尸回调：判别新旧**必须用 `OnStartedLeading` 收到的 term 参数**（定格于调用瞬间），不得在回调中途重读 `Elector.Term()`（重新竞选后该值会变）。

## 锁装配约束

- **一个 Elector 独占一个 lock 实例**：禁止将同一 `*lock.Lock` 注入多个并发运行的 Elector（token 互相踩踏，续约/释放语义失效）
- **后端必须支持续期**（`lock.Backend` 实现 `lock.Renewer`），否则 Run 在首次续约时以 `lock.ErrRenewUnsupported` 终止
- **使用默认 FailClosed 策略**：leader 对 "err 非 nil" 的返回值一律不信任其 `ok` 分量——FailOpen 的放行组合 `(true, err)` 会被忽略（防御误配导致全体实例同时"当选"双主）

## 参数约束建议

- 硬约束（`New` 校验，违反 panic）：`LeaseDuration > RenewDeadline > RetryPeriod > 0`
- 软建议：`LeaseDuration − RenewDeadline`（默认 15s − 10s = 5s 余量）应 **≥ 预估时钟偏差 + 最大 GC/调度停顿**；锁的过期由存储侧 TTL 裁决，本地钟只影响续约时机
- `lock.WithTTL` 与 `WithLeaseDuration` 保持一致（见 Option 表）
