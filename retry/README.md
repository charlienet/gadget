# retry

上下文感知的通用重试执行器，仅依赖标准库（`go.mod` 无任何 require）。

三要素模型——一次重试行为由三个正交要素完全决定：

- **退避策略（Backoff）**：两次尝试之间等待多久。内置 `Fixed`、`Exponential`，以及 `FullJitter` / `EqualJitter` 两种抖动包装器
- **终止条件（Option）**：重试到什么时候停止。`WithMaxAttempts` 限制 fn 总执行次数（含首次，默认 5）；`WithMaxElapsed` 限制总耗时（软上限，默认禁用）；两者无优先关系，每轮检查、先到先终止
- **错误分类（WithRetryable）**：决定哪些错误值得重试，默认全部错误可重试；判定为不可重试的错误立即原样返回

## 安装

```bash
go get github.com/charlienet/gadget/retry@v0.1.0
```

## 快速开始

```go
import (
    "context"
    "time"

    "github.com/charlienet/gadget/retry"
)

// 零配置：指数退避（100ms 起 ×2 封顶 30s）、最多 5 次尝试、全部错误可重试
err := retry.Do(ctx, func(ctx context.Context) error {
    return flakyCall(ctx)
})

// 显式配置：固定间隔 + 次数上限
err = retry.Do(ctx, fn,
    retry.WithBackoff(retry.Fixed(10*time.Millisecond)),
    retry.WithMaxAttempts(3),
)
```

## 核心 API

### Do

```go
func Do(ctx context.Context, fn func(ctx context.Context) error, opts ...Option) error
```

按退避策略反复执行 fn，直到成功、终止条件耗尽或 ctx 取消。

### 退避策略

| 构造函数 | 序列 | 非法参数（构造期 panic） |
|---|---|---|
| `Fixed(d)` | 恒为 `d` | `d <= 0` |
| `Exponential(initial, multiplier, max)` | `initial` 起，每步 `min(上一步×multiplier, max)`——先乘后 clamp，杜绝 float64 溢出 | `initial <= 0`、`multiplier < 1`、`max < initial` |
| `FullJitter(b)` | 在 `[0, b 当前值)` 内均匀随机（AWS 风格） | `b == nil` |
| `EqualJitter(b)` | `d/2 + rand[0, d/2)`——一半固定、一半随机 | `b == nil` |

抖动包装器的随机源为 `math/rand`（Go 1.20+ 自动播种）。`Backoff` 接口：

```go
type Backoff interface {
    NextBackOff() time.Duration // 返回值 <= 0 时 Do 视为 0（立即重试，但仍检查 ctx）
    Reset()                     // Do 每次执行入口调用，序列从头开始
}
```

实例非并发安全；使用默认配置时无需关心——`Do` 每次调用内部新建独立实例。

### Option

| Option | 默认值 | 非法值处理 | 说明 |
|---|---|---|---|
| `WithBackoff(b)` | `Exponential(100ms, 2, 30s)` | `b == nil` 忽略 | 传入实例仅在单次 Do 内使用；多 goroutine 并发 Do 请各自构造，不要共享 |
| `WithMaxAttempts(n)` | `5` | `n <= 0` 忽略 | fn 总执行次数上限（含首次）：`n=3` → 首次 + 至多 2 次重试 |
| `WithMaxElapsed(d)` | `0`（禁用） | `d <= 0` 忽略 | 总耗时软上限，检查发生在每轮重试之前 |
| `WithRetryable(fn)` | 全部错误可重试 | `fn == nil` 忽略 | 判定 false 的错误立即终止并原样返回 |

注意与多数配置包不同：非法 Option 值**静默忽略**（保持默认），而非 panic；panic 只发生在构造函数参数非法时。

## 返回语义

`Do` 的四条规则（源码 `retry.go`，均有测试佐证）：

1. fn 某次返回 nil → 立即返回 nil（**成功优先**，即使 ctx 已取消）；
2. 错误不可重试（`WithRetryable` 判定 false），或 MaxAttempts / MaxElapsed 耗尽 → 返回**最后一次 fn 的原始错误**（不包装，`errors.Is` / `errors.As` 保真）；
3. ctx 在尝试之间或退避睡眠中取消/超时 → 返回 `ctx.Err()`；
4. ctx 在 fn 执行中取消 → 不中断 fn（无法做到），待其返回后按规则 1/2/3 处理。

其他边界：

- fn panic 直接穿透，本包不做 recover；
- fn 为 nil 时 panic（fail-fast，与构造期校验一致）；
- MaxElapsed 是软上限：睡眠不被截断，实际耗时可能超出上限**至多一个退避间隔**；
- 对共享 Backoff 实例顺序调用两次 Do，入口各自 Reset，退避序列均从头开始。

## 与 gadget/redis 对接

用 `IsUnavailable` 精确圈定重试范围——仅连接/服务类故障重试，命令级错误（语法错、WRONGTYPE 等）不浪费退避时间：

```go
err := retry.Do(ctx, func(ctx context.Context) error {
    return client.Set(ctx, "k", "v", 0).Err()
}, retry.WithRetryable(func(err error) bool {
    return redis.IsUnavailable(err) // 仅连接/服务不可用类错误重试
}))
```
