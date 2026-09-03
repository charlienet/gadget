# gadget

一套 Go 基础设施组件集：常用模块的统一实现与充分测试，供个人项目跨项目复用，避免每写一个项目就重复实现一遍缓存、配置、消息、分布式锁等基础能力。

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
| `github.com/charlienet/gadget/config` | 配置管理：支持环境变量、文件与远程配置源（etcd、nacos、consul） |
| `github.com/charlienet/gadget/store` | 存储抽象 |
| `github.com/charlienet/gadget/logger` | 日志组件：基于 slog 的默认实现与扩展能力 |
| `github.com/charlienet/gadget/id_generator` | ID 生成器 |

### 插件

核心模块定义接口，`plugins/` 目录提供具体实现：

| 插件 | 实现的接口 |
|---|---|
| `plugins/cache/redis` `bigcache` `freecache` `gcache` | `cache.Store`（Redis / 内存 LRU 后端） |
| `plugins/lock/redis` | `lock.Backend` + `lock.Renewer`（SETNX + Lua 原子释放/续期） |
| `plugins/ratelimit/redis` | `ratelimit.Backend`（GCRA 批发脚本，BestEffort 租约 / AllOrNothing 精确双模式） |
| `plugins/broker/kafka` `nats` `rabbitmq` `redis` | `broker` 消息代理 |
| `plugins/config/source/consul` `etcd` `nacos` | `config` 远程配置源 |
| `plugins/store/consul` `file` `redis` | `store` 存储后端 |

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

- 多模块工作区：根目录 `go.work` 纳入全部 29 个模块；`go build ./...` 默认走 workspace
- **发布前必须用 `GOWORK=off` 验证单模块**（workspace 会掩盖模块图问题）：`GOWORK=off go build ./... && go vet ./... && go test ./...`
- tag 规范：`<模块相对路径>/v<semver>`（如 `cache/v0.4.1`、`plugins/cache/redis/v0.1.11`），tag 打在评审通过的提交上
