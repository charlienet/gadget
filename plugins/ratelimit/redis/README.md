# plugins/ratelimit/redis

`ratelimit.Backend` 的 Redis 实现：以 GCRA 令牌桶 Lua 脚本作为多实例共享的"远程批发通道"。速率配置随 `ratelimit.Spec` 逐次下发（后端无状态，杜绝双配置源），零售/租约/兜底逻辑全部在 `gadget/ratelimit` core。

## 安装

```bash
go get github.com/charlienet/gadget/plugins/ratelimit/redis@latest
```

## 用法

包名 `redis` 与 go-redis 冲突，import 时起别名 `redislimit`：

```go
import (
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/charlienet/gadget/ratelimit"
	redislimit "github.com/charlienet/gadget/plugins/ratelimit/redis"
)

rdb := goredis.NewClient(&goredis.Options{
	Addr:       "127.0.0.1:6379",
	DialTimeout: 2 * time.Second, // 拨码/读写超时归 go-redis client 配置职责
})

limiter := ratelimit.New(redislimit.New(rdb),
	ratelimit.WithRate(100, time.Minute),
	ratelimit.WithBurst(200),
)
ok, err := limiter.Allow(ctx, "user:42", 1) // 批发经 Redis GCRA 脚本，本地租约期内零网络

defer limiter.Close() // ⚠ 会经本包 Close 关闭注入的 rdb（见下方生命周期警示）
```

> **client 生命周期警示**：本包实现 `io.Closer`，`ratelimit.Limiter.Close()`（含其对本包 `Close()` 的转调）**会关闭注入的 go-redis client**。若该 client 被多个组件共享（cache / lock / 其他 limiter 实例等），不要依赖 `Limiter.Close` 释放连接——应由组合根（main / 依赖注入容器）统一管理 client 生命周期；需要由 Limiter 负责释放时，请给它独配的 client 实例。

多实例部署时各实例创建相同配置的 Limiter 即共享同一份 Redis 全局配额。租约模式下全局瞬时突发上界为 **(实例数+1)×Burst**（各实例本地租约存量叠加远端桶容量），详见 `ratelimit` 包 doc 的披露；要求全局严格配额用 `ratelimit.WithoutLocalLease()`。

### Option

| Option | 默认值 | 说明 |
|---|---|---|
| `WithKeyPrefix(prefix)` | `"rate:"` | 限流 key 命名空间前缀（与 `gadget/redis` RateLimiter 的 `"rate:"` 约定一致，便于平滑迁移）；空字符串防御式忽略 |

不设拨码/超时类 Option：那属于 go-redis client 自身配置；core 侧的每次批发超时由 `ratelimit.WithBackendTimeout` 约束。

## 授予语义（脚本按 `mode` 参数分支）

- **BestEffort（租约批发，mode=0）**：`remaining < cost` 时裁剪为 `math.floor(remaining)` 授予——保证**扣减量 == 返回量**；
- **AllOrNothing（精确模式，mode=1）**：`remaining < cost` 时直接拒绝（`granted=0`），**不推进 TAT、不执行 SET**，供配额/计费不可蒸发场景；拒绝后旧 key 按 `EX=reset_after` 恰在桶回满时刻过期，过期后 `GET nil→'0'→tat=now` 满桶重建，语义等价无跳变。

返回值映射：`granted =` 脚本实际消耗的 cost；`retryAfter =` 未足额时脚本提示的最早重试时刻（足额为 0）。

## 错误契约（对齐 lock.Backend 三条）

| Redis 错误 | 处理 |
|---|---|
| 连接拒绝 / `io.EOF` / 网络超时 / `redis: connection pool timeout` / `redis: client is closed` 等连接服务层故障（`isUnavailable` 判定） | 包装 `ratelimit.ErrBackendUnavailable`，交 core 按 FailPolicy 兜底 |
| 命令级错误（Lua 运行错误、TAT 值损坏等） | **原样透传，不兜底**（防配置/数据错误被 FailOpen 掩盖） |
| ctx 取消/超时 | **原样返回 `ctx.Err()`**，绝不包装为不可用 |

## 与 gadget/redis 模块现有 GCRA 脚本的关系（同源提醒）

本包脚本以 `redis/ratelimit.go` 的 `tokenBucketAtMostScript`（公共 API 已冻结，原脚本不动）为基础**复制改造**，三处有意差异：

1. `ARGV[1]=Spec.Burst`——原调用 burst 恒等于 rate，改造版 burst 与 rate 解耦、由 Spec 显式下发；
2. BestEffort 裁剪分支先 `math.floor` 再推进 TAT（原脚本以小数 cost 推进，RESP integer 向下截断，每次部分授予蒸发 ≤1 令牌）；
3. 新增 AllOrNothing 分支（`mode=1`，不足额拒绝且零写入）。

其余（`redis.call('TIME')` 取时 + jan_1_2017 基准、`replicate_commands`、`SET EX ceil(reset_after)` 闲置自动过期）逐字保留原脚本。**两份脚本内容同源：修改任何一方的公共语义（TIME 基准、返回结构、EX 策略）时必须同步评估另一方。** 集成测试内置原脚本逐字副本作为 BestEffort 对照基线（唯一预期差异 = floor 蒸发），修改脚本会直接被对照测试拦截。

远端闲置回收无需配置：`SET EX ceil(reset_after)` 使 key 在桶自然耗尽时自动过期（GCRA 内建属性），`Spec.IdleRetention` 在本插件被忽略。

## 测试

```bash
REDIS_URL=redis://127.0.0.1:6379 go test ./...
```

未设置 `REDIS_URL` 时集成测试整体 `t.Skip`（单元测试仍可跑：New(nil) panic、返回解析、不可用判定表、ctx 透传）；设置但不可达则直接失败，避免假绿。

## 发布依赖

`github.com/charlienet/gadget/ratelimit` 当前经仓库根 `go.work` 以 workspace 成员解析（本模块 `go.mod` 暂不 require 该路径：其 `ratelimit/v0.1.0` tag 尚未发布，写死不存在版本会破坏模块图）。正式发布顺序：推送 core tag → 本模块 `go mod tidy` 补 require 与 go.sum → `GOWORK=off` 验证 → 推送本模块 tag。
