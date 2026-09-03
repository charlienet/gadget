# ratelimit

后端可插拔的限流器：单机限流与多实例分布式限流，默认采用"远程批发、本地零售"租约模式大幅降低远端调用频率。仅依赖标准库（`go.mod` 无任何 require）。

与 `gadget/redis` 的 GCRA 限流（公共 API 已冻结、强绑定 go-redis）互补：本包把"速率语义"下沉到可替换的 `Backend`，核心只做零售逻辑——本地账本、批发合并、静默期、错误分诊、失效兜底。

## 安装

```bash
go get github.com/charlienet/gadget/ratelimit@latest
```

## 两种模式

```
租约模式（默认）：
  Allow ──热路径──▶ 本地账本扣减（纯内存，零网络）
     │ 存量不足
     ▼
  per-key in-flight 合并 ──leader──▶ Backend.Wholesale(BestEffort) 一整批
     └─followers 共享结果─◀ 注入 remain / granted==0 进入静默期

精确模式（WithoutLocalLease）：
  Allow ──每次──▶ Backend.Wholesale(AllOrNothing) 严格全局配额
```

| 维度 | 租约模式（默认） | 精确模式 |
|---|---|---|
| 远端调用 | 每 key 约每 `LeaseInterval` 一次（并发合并） | 每次 Allow 一次 |
| 全局速率 | 长期严格受 Spec 约束 | 严格逐次 |
| 瞬时突发 | **≤ (实例数+1)×Burst**（见下方披露） | ≤ Burst |
| 授予语义 | `GrantBestEffort`（能租多少租多少） | `GrantAllOrNothing`（不足额拒绝且**不扣减**，配额/计费不可蒸发场景） |
| 后台协程 | 1 个闲置回收 sweeper | 无 |

本地租约账本是**纯存量**：`remain` 只能被批发 granted 注入、被 Allow 扣减，不存在任何按速率的自补充——速率语义 100% 由 Backend 的桶决定，杜绝"本地+远端双重发币"导致的速率翻倍。

### 突发上界披露（选租约模式的知情项）

租约模式下各实例本地持有整批租约，**全局瞬时突发上界为 (实例数 + 1) × Burst**：各实例本地存量之和（至多 实例数 × Burst，批发量 `want` 被 clamp 到 Burst）叠加远端桶自身的突发容量（Burst）。且后端状态按 `~burst/rate` 秒量级自然过期/回补，周期性将远端重置为满桶——长期速率仍严格受 Spec 约束，但短窗口放行量可超过单机视角。要求全局严格配额时用 `WithoutLocalLease()`。

## 快速开始

```go
import "github.com/charlienet/gadget/ratelimit"

// 单机
limiter := ratelimit.New(ratelimit.Memory(),
    ratelimit.WithRate(100, time.Minute),
    ratelimit.WithBurst(200),
)
defer limiter.Close()

ok, err := limiter.Allow(ctx, "user:42", 1)
if err != nil && errors.Is(err, ratelimit.ErrExceeded) {
    return errors.New("请求过于频繁")
}

// 阻塞等待（总时长受 WithMaxWait 约束）
if err := limiter.Wait(ctx, "user:42", 1); err != nil {
    return err
}
```

多实例分布式限流由 `plugins/ratelimit/redis` 插件提供 `Backend` 实现（GCRA 批发脚本，包名与 go-redis 冲突，import 建议起别名 `redislimit`）：

```go
import (
	goredis "github.com/redis/go-redis/v9"
	redislimit "github.com/charlienet/gadget/plugins/ratelimit/redis"
)

rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379"})
limiter := ratelimit.New(redislimit.New(rdb),
	ratelimit.WithRate(100, time.Minute),
	ratelimit.WithBurst(200),
)
defer limiter.Close() // 会经插件 Close 释放 rdb 连接资源
```

不同速率 / 多租户 = 建多个 `Limiter` 实例（v1 无 Configure 规则表）。

## 核心 API

| API | 说明 |
|---|---|
| `New(b Backend, opts ...Option) *Limiter` | 创建限流器；`b == nil` panic；非法 Option 值防御式忽略保持默认 |
| `(*Limiter) Allow(ctx, key, n) (bool, error)` | 非阻塞消耗 n 个令牌；单次至多一次批发；参数见下方契约 |
| `(*Limiter) Wait(ctx, key, n) error` | 阻塞等待；四出口：成功 nil / ctx 取消 `ctx.Err()` / 超 `WithMaxWait` ErrExceeded 语义错误 / 后端不可用按 FailPolicy 立即返回错误（不循环） |
| `(*Limiter) Close() error` | 幂等停止 sweeper；Backend 实现 `io.Closer` 时释放其资源；此后 Allow/Wait 返回 `ErrClosed` |
| `Execute[T](ctx, l, key, fn)` | 泛型执行器，固定消耗 1 个令牌（n>1 用 Allow）；超限时不调用 fn；FailOpen 兜底放行时仍执行 fn 并透传兜底错误 |
| `Memory() Backend` | 内置单机后端：per-key 惰性补充桶，共享实例即共享配额 |

### Backend 接口（批发通道）

```go
type Backend interface {
    Wholesale(ctx context.Context, key string, want int, spec Spec, mode GrantMode) (granted int, retryAfter time.Duration, err error)
}
```

实现者必须无状态配置（速率随 `Spec` 下发），错误契约三条（对齐 `lock.Backend`）：

1. 后端不可用 → 必须包装 `ErrBackendUnavailable`（核心据此走 FailPolicy 兜底）；
2. 其余错误（命令级、Lua 运行错误）→ 原样返回，核心透传不兜底；
3. ctx 取消/超时 → 原样返回 `ctx.Err()`，**不得**包装为不可用。

可选实现 `io.Closer`——仅用于释放连接等资源，与令牌归还无关（不做 giveback）。注意：`Limiter.Close` 会转调它，插件后端将因此**关闭注入的 go-redis client**；client 被多组件共享时，其生命周期应由组合根统一负责，不要依赖 `Limiter.Close` 释放连接（需要时给每个 Limiter 配独立 client）。

## 错误契约（分诊表）

| 来源 | Allow 结果 | 判定 |
|---|---|---|
| 本地存量不足 / 静默期 / 批发后仍不足 / Wait 超 MaxWait | `(false, *ExceededError)` | `errors.Is(err, ErrExceeded)`；`errors.As` 可取 `Key/N/RetryAfter` |
| ctx 取消/超时 | `(false, ctx.Err())` | 透传，**不进 FailPolicy** |
| 后端不可用 + FailOpen（默认） | `(true, err)` | `errors.Is(err, ErrFailOpen)` 且 `errors.Is(err, ErrBackendUnavailable)` 双可判——放行但可感知 |
| 后端不可用 + FailClosed | `(false, err)` | `errors.Is(err, ErrBackendUnavailable)` |
| 命令级其他错误 | `(false, err)` | 原样透传，防配置错误被兜底掩盖 |
| 参数错误（`key==""` / `n<=0` / `n>Burst`） | `(false, ErrInvalidArgument)` | fail-fast，先于一切路径、不触后端 |
| Limiter 已 Close | `(false, ErrClosed)` | 不进 FailPolicy、不触后端 |

`n > Burst` 视为参数错误而非静默钳制："本实例任何时刻都无法满足"，调用方应改代码或调大 `WithBurst`。

## Option

| Option | 默认值 | 非法值处理 | 说明 |
|---|---|---|---|
| `WithRate(n, per)` | `100/1s` | `n<=0` 或 `per<=0` 忽略 | 速率：per 窗口内 n 个令牌 |
| `WithBurst(n)` | `2×Rate` | `n<=0` 忽略 | 桶容量（突发容忍），随最终 Rate 推导 |
| `WithMaxWait(d)` | `30s` | `d<=0` 忽略 | Wait 总等待上限 |
| `WithoutLocalLease()` | — | — | 切换精确模式（默认即租约模式） |
| `WithLeaseInterval(d)` | `1s` | `d<=0` 忽略 | 目标批发间隔（want 公式的 d） |
| `WithLeaseRatio(r)` | `0.5` | `r<=0` 或 `r>1` 忽略 | 批量调节系数：小省远端、大贴精确 |
| `WithBackendTimeout(d)` | `min(LeaseInterval, 5s)` | `d<=0` 忽略 | core 内部批发 ctx 超时（归 core 非插件） |
| `WithIdleRetention(d)` | `60s` | `d<=0` 忽略 | 本地账本闲置回收阈值；随 `Spec.IdleRetention` 下发（Memory 用，GCRA 类忽略） |
| `WithFailPolicy(p)` | `FailOpen` | — | 后端不可用兜底；`FailOpen = iota` 零值即默认放行 |
| `WithLogger(l)` | `slog.Default()` | `nil` 忽略 | FailOpen 兜底等内部事件记录 |
| `WithClock(c)` | 系统时钟 | `nil` 忽略 | `Clock{ Now() time.Time }`，测试免 sleep |

批发批量公式：`want = clamp(round(Rate × LeaseInterval / Per × LeaseRatio), 1, Burst)`。

默认 FailOpen 的取舍（对齐 `redis/ratelimit.go`"限流默认 FailOpen"）：限流是保护性能力，服务不可用时宁可多放不阻塞业务。与 lock 的默认（FailClosed）相反，系语义差异——锁错放行破坏互斥，限流错放行仅放大流量，且调用方可经 `ErrFailOpen` 感知。

## 并发说明

- `Limiter` 全部方法并发安全。锁纪律：per-key 账本条目各持一把互斥锁，**临界区内只做三件事**——判存量、判静默期、登记/消费 pending，绝不做网络等待；
- 批发在途期间，同 key 存量充足的热路径请求与其他 key 的请求均不被阻塞；
- in-flight 合并为自研最小实现（per-key pending + chan 广播），零第三方依赖；
- leader 批发用内部 ctx（`context.WithTimeout(context.Background(), BackendTimeout)`），单个请求 ctx 取消不殃及共享结果的其他请求；
- sweeper 为单后台协程（`Once + stopChan + WaitGroup` 受控退出，对齐 cache 先例，不加 recover），删除条目前持有目标条目锁并复检 `idleAt`（宁可延后一轮，不误删在途/刚触碰的条目）；
- 闲置回收仅租约模式需要；Memory 后端不建后台协程（桶在 Wholesale 时惰性判 idle 并重置）。

## 明确不做

giveback 租约归还（崩溃浪费上界 = 实例数 × 批量，靠"批量小 + 后端过期"消化）、Configure / per-key 规则表、漏桶/滑动窗口变体、retry/breaker 组合层、metrics 出口。

## 与 redis 模块现有 GCRA 实现的关系

`gadget/redis` 的 `RateLimiter`（公共 API 冻结）与本包插件共享同一脚本源头：`plugins/ratelimit/redis` 以迁移改造的 `tokenBucketAtMostScript` 实现批发（burst 与 rate 解耦、新增 AllOrNothing 分支、BestEffort 裁剪 floor）。两份脚本内容同源，未来修改需互相同步（插件集成测试内置原脚本逐字副本作为对照基线兜底）。
