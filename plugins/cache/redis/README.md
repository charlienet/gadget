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

防止缓存雪崩——同一批写入的 key 过期时间分散开：

```go
c := cache.New(
    rediscache.New(rdb,
        rediscache.WithTTLFactor(30), // 默认 30，每次 Put 加 [1,30] 秒随机
    ),
)
```

- `ttlFactor=0`：关闭随机化
- `ttlFactor=30`：每次写入时在 TTL 基础上加 1~30 秒随机偏移

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

PubSub 监听器内置健康检查（每 15 秒 Ping）和自动重连。

### Stream 监听器（可靠投递）

基于 Redis Stream + Consumer Group，**至少一次送达**。重启后自动重放未确认消息：

```go
import (
    "github.com/charlienet/gadget/plugins/cache/redis/listener/stream"
)

lis := stream.NewStreamListener(rdb,
    stream.WithStreamName("cache:invalidate"),
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
| 消费者竞争 | ❌ 广播模式 | ✅ Consumer Group 负载均衡 |

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
