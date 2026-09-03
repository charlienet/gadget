# gadget

一套 Go 基础设施组件集：常用模块的统一实现与充分测试，供个人项目跨项目复用，避免每写一个项目就重复实现一遍缓存、消息、分布式锁、限流、熔断、选举、优雅关闭等基础能力。

## 模块总览

本仓库为多模块仓库，每个子目录是独立的 Go module，独立发版、按 semver 独立演进。

### 核心模块

| 模块路径 | 说明 |
|---|---|
| `github.com/charlienet/gadget/cache` | 缓存组件：L1 内存缓存（LRU、热 key 容量驱逐、TTL jitter）+ 可插拔远程 Store，二级缓存语义 |
| `github.com/charlienet/gadget/redis` | Redis 客户端包装：失效兜底机制、熔断器、优雅关闭、多模式 URL 解析；`IsUnavailable` 连接类错误判定 |
| `github.com/charlienet/gadget/lock` | 分布式锁：基于 `Backend` 抽象（TryAcquire/Release/Renew），token 防误删/误续，后端不可用时按 FailPolicy（FailClosed/FailOpen）兜底 |
| `github.com/charlienet/gadget/ratelimit` | 限流器：后端可插拔（`Backend` 批发通道 + `Memory()` 单机），默认"远程批发、本地零售"租约模式，支持精确模式（AllOrNothing 防蒸发），FailOpen/FailClosed 兜底 |
| `github.com/charlienet/gadget/broker` | 消息代理抽象 |
| `github.com/charlienet/gadget/logger` | 日志组件：基于 slog 的默认实现与扩展能力 |
| `github.com/charlienet/gadget/id_generator` | ID 生成器 |
| `github.com/charlienet/gadget/breaker` | 通用三态熔断器（Closed/Open/HalfOpen），Execute 一站式与 TwoStep hook 形态，纯标准库零依赖 |
| `github.com/charlienet/gadget/leader` | 基于分布式锁的 leader 选举状态机（LeaseDuration/RenewDeadline/RetryPeriod + 任期 term） |
| `github.com/charlienet/gadget/lifecycle` | 进程优雅关闭编排（注册逆序串行、Component 幂等契约、错误聚合） |
| `github.com/charlienet/gadget/retry` | context 感知重试执行器（退避 × 终止条件 × 错误分类） |

### 插件

核心模块定义接口，`plugins/` 目录提供具体实现：

| 插件 | 实现的接口 |
|---|---|
| `plugins/cache/redis` `bigcache` `freecache` `gcache` | `cache.Store`（Redis / 内存 LRU 后端） |
| `plugins/lock/redis` | `lock.Backend` + `lock.Renewer`（SETNX + Lua 原子释放/续期） |
| `plugins/ratelimit/redis` | `ratelimit.Backend`（GCRA 批发脚本，BestEffort 租约 / AllOrNothing 精确双模式） |
| `plugins/broker/kafka` `nats` `rabbitmq` `redis` | `broker` 消息代理 |

示例：使用 Redis 作为缓存的远程 Store：

```go
import (
    "github.com/charlienet/gadget/cache"
    cacheredis "github.com/charlienet/gadget/plugins/cache/redis"
    "github.com/charlienet/gadget/redis"
)

rdb, err := redis.NewWithUrl("redis://127.0.0.1:6379")
if err != nil {
    panic(err)
}
c := cache.New(cache.WithStore(cacheredis.New(rdb)))
```

### 废弃与归档模块（abandoned）

以下模块方向已永久关闭，代码已物理删除（可从 git 历史找回），已发布 tag 一律原地封存、不发新版：

| 模块 | 处置说明 |
|---|---|
| `store` + `plugins/store/consul` `file` `redis` | 存储抽象，代码已物理移除；已发布的 `store/*`、`plugins/store/*` tag 原地封存 |
| `config` + `plugins/config/source/consul` `etcd` `nacos` | 配置管理为空壳实现、从未有功能，该赛道无生态位；配置管理需求直接使用 koanf / viper 等成熟库，本库永久关闭该方向；已发布的 `config/*` 与 `plugins/config/*` tag（共 11 个）原地封存不发新版 |

## 测试

单元测试离线可跑，不依赖外部服务。

Redis 相关测试通过环境变量守卫接入真实实例，未设置时测试会 **SKIP 并输出警告**：

| 环境变量 | 用途 |
|---|---|
| `REDIS_URL` | 单机/哨兵地址（`redis://:pass@host:6379`，支持逗号分隔多地址） |
| `REDIS_STACK_URL` | Redis Stack（含 RedisBloom/ReJSON 等模块） |
| `REDIS_CLUSTER_ADDRS` | Cluster 节点地址（逗号分隔） |
| `REDIS_PASSWORD` | 密码（可选，URL 已含密码时无需设置） |

`redis/test` 子包提供统一的测试入口（`RunOnRedis` / `RunOnRedisStack` / `RunOnRedisCluster` / `RunOnMiniRedis`），可在自己的测试中直接复用：

```go
import test "github.com/charlienet/gadget/redis/test"

func TestSomething(t *testing.T) {
    test.RunOnRedis(t, func(rdb redis.Client) {
        // 真实 Redis 上执行的断言；REDIS_URL 未设置时自动跳过
    })
}
```

## 仓库结构与开发

- 多模块工作区：根目录 `go.work` 纳入全部 21 个模块；`go build ./...` 默认走 workspace
- **发布前必须用 `GOWORK=off` 验证单模块**（workspace 会掩盖模块图问题）：`GOWORK=off go build ./... && go vet ./... && go test ./...`
- tag 规范：`<模块相对路径>/v<semver>`（如 `cache/v0.4.1`、`plugins/cache/redis/v0.1.11`），tag 打在评审通过的提交上
