# breaker

通用三态熔断器（Closed / Open / HalfOpen），仅依赖标准库（`go.mod` 无任何 require）。

状态机提取自 `gadget/redis` 的熔断实现（逐分支语义等价），与任何具体客户端解耦：什么错误算"服务故障"由注入的 `Classifier` 决定，熔断逻辑可复用于数据库、HTTP、消息队列等一切"调用—结果"场景。

## 安装

```bash
go get github.com/charlienet/gadget/breaker@v0.1.0
```

## 三态模型

```
                 连续失败达阈值（默认 3）
     ┌───────────────────────────────────────┐
     ▼                                       │
 ┌────────┐   冷却期满(1s)·放行单飞探测   ┌──────────┐
 │ Closed │ ────────────────────────────▶ │ HalfOpen │
 │ 正常放行 │◀──────────────────────────── │  单飞探测  │
 └────────┘   探测成功 / 中性错误           └──────────┘
                    （服务可达）→ 闭合              │
     ┌────────┐                                    │ 探测失败
 ───▶│  Open  │◀───────────────────────────────────┘
     │快速失败│   （回 Open 重置冷却）
     └────────┘
```

- **Closed**：正常放行；连续失败计数达阈值 → Open；成功清零计数；
- **Open**：**快速失败**——`Allow` 原样返回最近一次错误（不实际执行调用，避免每次请求都等连接超时）；冷却期（惰性判断，无定时器 goroutine）结束 → HalfOpen；
- **HalfOpen**：单飞探测——并发下同时只放行一个探测请求，其余快速失败；探测成功 → Closed（自动恢复）；探测失败 → 回 Open（重置冷却）。

## 快速开始

```go
import "github.com/charlienet/gadget/breaker"

b := breaker.New(
    breaker.WithThreshold(5),
    breaker.WithCooldown(2*time.Second),
    breaker.WithClassifier(isServiceErr), // 可选：精确圈定计入熔断的错误
)

// 推荐形态：一站式包装，自动上报结果
val, err := breaker.Execute(b, func() (int, error) {
    return riskyCall()
})
if err != nil {
    // 熔断拒绝时 err 即最近一次失败原文（errors.Is/As 保真）
}
```

## 两种使用形态

### Execute（推荐）

```go
func Execute[T any](b *Breaker, fn func() (T, error)) (T, error)
```

`Allow` 判断 + 自动 `Report`：拒绝时返回（零值, lastErr）且 fn 不执行；fn 返回后按 Classifier 自动上报。**从结构上消除 TwoStep 的探测泄漏风险**，普通业务调用一律优先用它。

### TwoStep（hook/中间件场景）

调用点与结果回调点不在同一函数栈时（如 go-redis hook、HTTP 中间件）手动两步：

```go
if err := b.Allow(); err != nil {
    return err // 快速失败
}
err := doWork()
b.Report(err) // 必须最终调用！见下方契约
```

> **TwoStep 契约**：`Allow` 返回 nil 后，**必须最终调用 `Success` / `Fail` / `Report` 之一**。漏报的后果：半开探测的在途标记（单飞）永不释放，后续所有 `Allow` 持续被拒——熔断器卡死在半开。无法保证回调路径全覆盖时用 `Execute` 代替。

## 核心 API

| API | 说明 |
|---|---|
| `New(opts ...Option) *Breaker` | 创建熔断器；非法 Option 值一律忽略保持默认 |
| `(*Breaker) Allow() error` | 放行判断：`nil` = 放行；拒绝**原样返回 lastErr**（不包装、无哨兵错误） |
| `(*Breaker) Success()` | 记录成功：清零计数；HalfOpen → Closed |
| `(*Breaker) Fail(err error)` | 记录失败：达阈值 → Open；半开探测失败 → 回 Open 重置冷却 |
| `(*Breaker) Report(err error)` | 按 Classifier 三分类记录（见下） |
| `(*Breaker) State() State` | 状态快照（只读，不触发 Open→HalfOpen 惰性转换） |
| `Execute[T](b, fn)` | 一站式包装（见上） |
| `State.String()` | `"closed"` / `"open"` / `"half-open"` |

### Report 三分支

```go
type Classifier func(err error) bool // true = 计为熔断失败；默认：所有非 nil 错误计入
```

| 输入 | 行为 |
|---|---|
| `err == nil` | `Success()`：清零计数，半开探测成功 → Closed |
| `classifier(err) == true` | `Fail(err)`：计入连续失败，达阈值触发 Open |
| 其余（非计数错误） | **中性**：Closed 下不计入也不干扰（保留既有计数，"连续失败"只看计数错误）；**HalfOpen 下 → Closed**——非计数错误（如命令级 WRONGTYPE）证明服务可达，闭合恢复并清零（语义同 gobreaker 的 IsSuccessful） |

默认 Classifier 是"所有非 nil 错误计为失败"；对接具体客户端时应注入精确判定，例如配合 `gadget/redis`（仅连接/服务类故障计入，命令级错误不拖垮熔断）：

```go
b := breaker.New(breaker.WithClassifier(redis.IsUnavailable))
```

### Option

| Option | 默认值 | 非法值处理 | 说明 |
|---|---|---|---|
| `WithThreshold(n)` | `3` | `n <= 0` 忽略 | 连续失败阈值（成功清零计数） |
| `WithCooldown(d)` | `1s` | `d <= 0` 忽略 | Open 冷却期，惰性判断 |
| `WithClassifier(c)` | 全部非 nil 计入 | `c == nil` 忽略 | 失败判定，true = 计入熔断 |

注意：非法 Option 值**静默忽略**（保持默认），不 panic。

明确不做（保持最小语义）：MaxRequests（半开多探测并发数）、Interval（滑动窗口/计数代际清零）、Name、ReadyToTrip / OnStateChange 回调。

## panic 注意事项

`Execute` 的 fn panic 时**直接穿透、不做任何计数**（与 `retry.Do` 哲学一致，本包不 recover）。但注意：若 panic 发生在**半开探测**中，探测标记（单飞）会因未上报而滞留，后续 `Allow` 持续拒绝。调用方应自行保证 fn 不 panic；无法保证时在捕获 panic 后自行补一次 `b.Report(...)` 解除泄漏。

## 为什么全部方法不带 ctx

`Allow` / `Success` / `Fail` / `Report` / `State` 乃至 `Execute` 一律不接收 `context.Context`，设计依据：

1. 纯内存状态机，无任何 IO，无取消语义可传递；
2. 无自身阻塞等待——冷却是 `Allow` 时的**惰性判断**（比较时间戳），不存在可取消的等待点；
3. `Breaker` 结构体不持 ctx 字段、无 `Close`，生命周期归调用方管理；
4. `Execute` 的 fn 若做 IO，由调用方闭包自带 ctx（Go 惯例：ctx 作首个非常量参数属于 IO 边界，不在此抽象层强加）。

## 并发安全

1. **所有方法并发安全**：状态与失败计数统一由一把互斥锁保护（状态切换与计数强相关，用锁保持一致，不依赖原子操作）；
2. **HalfOpen 单飞**：并发探测只放行一个请求，其余快速失败——不需要额外的限探测并发数配置；
3. **TwoStep 契约**（再强调）：`Allow` 放行后必须最终上报，否则半开探测泄漏（见上）；`Execute` 从结构上保证不泄漏。

冷却结束不会主动唤醒任何等待者：Open 期间若无人调用 `Allow`，状态停留在 Open 直到下一次 `Allow` 惰性判断——无定时器 goroutine，也就无资源泄漏。
