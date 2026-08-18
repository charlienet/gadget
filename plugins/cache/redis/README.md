# Redis Cache Store

Redis 缓存存储插件，提供基于 Redis 的分布式缓存能力，以及跨实例缓存失效通知机制。

## 使用方式

### 创建 Redis 缓存存储

```go
import (
    "github.com/charlienet/gadget/cache"
    rediscache "github.com/charlienet/gadget/plugins/cache/redis"
    "github.com/charlienet/gadget/redis"
)

rdb := redis.New()

c := cache.New(
    cache.WithMemStore(),              // 本地内存缓存（第一级）
    rediscache.New(rdb),               // Redis 远程缓存（第二级）
    cache.WithName("users"),           // 缓存名称，用作 Redis key 前缀
    cache.WithTTL(300),                // 默认过期时间 300 秒
)
```

> **TTL 防雪崩随机默认开启**：Put 的 expireSeconds>0 时自动叠加 +1~29s 随机偏移
> （同批 key 过期时间分散，防止缓存雪崩），零配置生效。需要 TTL 精确可控时可显式
> 关闭：`rediscache.WithTTLFactor(0)`（或 `(1)`），详见下文「TTL 随机化」。

### 基本操作

```go
ctx := context.Background()

// 写入（自动序列化为 JSON）
c.Put(ctx, "user:123", user, 3600)

// 读取
var u User
err := c.Get(ctx, "user:123", &u)

// 删除
c.Delete(ctx, "user:123")

// 缓存穿透 + 数据源加载
err = c.Getfn(ctx, "user:456", &u, func(ctx context.Context, key string, v any) (bool, error) {
    return db.FindByKey(key, v)  // 从数据库加载
}, 3600)
```

## TTL 随机化

> **TTL 防雪崩随机偏移默认开启**：Put 的 `expireSeconds>0` 时自动叠加 [1,29] 秒
> 随机（同批写入的 key 过期时间分散开，防止缓存雪崩），使用者零配置即可获得。
> 需 TTL 精确可控（所见即所得）时显式关闭：

```go
c := cache.New(
    rediscache.New(rdb,
        rediscache.WithTTLFactor(0), // 显式关闭：TTL 与传入值一致
    ),
)
```

- 默认 `ttlFactor=30`：开启随机，每次写入在 TTL 基础上叠加 **1~29 秒**随机偏移
  （`expireSeconds<=0` 永不过期，不叠加，不受影响）；
- `WithTTLFactor(n)`（n>1）：叠加 **1~n-1 秒**随机偏移（如 n=30 即 +1~29s）；
- `WithTTLFactor(0)` 或 `(1)`：关闭随机，TTL 所见即所得。

**与 cache 包 `WithTTLJitter` 的关系**：`cache.WithTTLJitter(d)` 作用于本地内存层
（L1）的 Put 抖动（0~d 随机），本插件的 `WithTTLFactor` 作用于 Redis 层（L2）的
TTL 随机加时。两者职责不同、互不干扰，**两层默认各自开启**（cache 包 L1 抖动
需显式 `WithTTLJitter` 开启，本插件 L2 默认开启），可分别用 `WithTTLJitter(0)` /
`WithTTLFactor(0)` 关闭，或组合使用（两层 TTL 均被随机化，防雪崩效果更强）。

## Name 前缀

通过 `cache.WithName(name)` 为当前缓存实例所有 key 添加统一前缀：

```go
c := cache.New(
    cache.WithName("users"),
    rediscache.New(rdb),
)
// 实际存储 key: "users:user:123"
c.Put(ctx, "user:123", data, 60)
```

`Initialize` 方法在缓存创建时自动将 name 注入到 Redis store。

## 跨实例失效通知

支持两种监听器，用于多实例部署时同步清除本地缓存。

> **定位声明**：Redis PubSub/Stream 通知为**多节点失效广播的兜底实现**（最多一次送达；
> 订阅建立前发布的消息会丢失——用 `Ready()` 等待就绪可规避启动时序窗口）。
> 生产环境建议使用 MQ（如 nats/kafka）实现 `cache.Listener`，接入更可靠的广播机制。

### PubSub 监听器（默认）

基于 Redis PubSub，消息即时广播，**最多一次送达**：

```go
import (
    "github.com/charlienet/gadget/plugins/cache/redis/listener/pubsub"
)

// 所有实例使用相同的 channel 名称
lis := pubsub.NewListener(rdb, "cache:invalidate")

c := cache.New(
    cache.WithMemStore(),
    rediscache.New(rdb),
    cache.WithListener(lis),
)

// 任一实例执行 Delete → 所有实例本地缓存同步清除
c.Delete(ctx, "user:123")
```

PubSub 监听器内置健康检查（每 15 秒 Ping）和自动重连。`Publish` 默认 2 秒超时，
可通过 `pubsub.WithPublishTimeout` 调整。

> **启动时序**：`NewListener` 异步建立订阅，返回时订阅可能尚未就绪；PubSub 广播为
> 最多一次送达，**新实例在订阅建立前发布的失效消息会丢失**（本地缓存残留旧数据
> 直到 TTL 过期）。新实例对外提供服务前可等待订阅就绪信号
> （`<-lis.(interface{ Ready() <-chan struct{} }).Ready()`，带调用方超时）。
> Stream 监听器无此问题——Stream 持久化 + 消费组可从最早位置补读。

### Stream 监听器（可靠投递）

基于 Redis Stream + Consumer Group，**至少一次送达**。重启后自动重放未确认消息。
与 PubSub 不同，每个实例使用**独立的 consumer group**（默认组名自动拼接
`hostname-pid` 后缀），因此每条失效消息都会被**广播**到所有实例——缓存失效
要求每实例都收到，而不是在同组消费者之间分摊：

```go
import (
    "github.com/charlienet/gadget/plugins/cache/redis/listener/stream"
)

lis := stream.NewStreamListener(rdb,
    stream.WithStreamName("cache:invalidate"),
    // WithConsumerGroup 仅设置组名“前缀”，最终组名 = name-hostname-pid，
    // 保证每个实例的 consumer group 互不相同（广播语义的前提）
    stream.WithConsumerGroup("my-group"),
    stream.WithConsumerID("instance-1"),
)

c := cache.New(
    cache.WithMemStore(),
    rediscache.New(rdb),
    cache.WithListener(lis),
)
```

| 特性 | PubSub 监听器 | Stream 监听器 |
|------|---------------|---------------|
| 送达保证 | 最多一次（丢消息不重试） | 至少一次（Pending 恢复） |
| 重启恢复 | ❌ 重启期间消息全丢 | ✅ 重放未 ACK 消息 |
| 消息堆积 | ❌ 无缓冲 | ✅ Stream 持久化 |
| 消费模式 | ❌ 广播模式 | ✅ 广播（每实例独立 consumer group） |

## 清理机制

### TTL 过期

Redis 层面原生支持 TTL，key 到期自动删除。配合 `WithTTLFactor` 分散过期时间。

### 版本同步（Pull 模式）

开启后台协程定期比对本地缓存和远端数据，发现不一致时自动修复：

```go
c := cache.New(
    cache.WithMemStore(),
    rediscache.New(rdb),
    cache.WithVersionSyncInterval(30*time.Second), // 后台每30秒检查一批 key
)
```

每次检查 100 个 key，逐 key 对比版本时间戳，仅在远端更新时刷新本地。

### 概率性校验（Access 模式）

每 N 次本地命中触发一次远端检查，发现数据已不一致时立即清理本地：

```go
c := cache.New(
    cache.WithMemStore(),
    rediscache.New(rdb),
    cache.WithVerifyEvery(10), // 每命中10次检查一次远端
)
```

### 优雅降级

Redis 不可用时自动进入降级模式，跳过远端操作仅用本地缓存：

```go
c := cache.New(
    cache.WithMemStore(),
    rediscache.New(rdb),
    cache.WithDegradeThreshold(3),               // 连续3次失败进入降级
    cache.WithDegradeRecoveryInterval(5*time.Second), // 每5秒探测恢复
)
```
