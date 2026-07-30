# 多级缓存 (Multi-Level Cache)

支持本地缓存和远程存储组成多级缓存机制，内置缓存穿透防护、并发控制（singleflight）、跨实例失效通知。

## 快速开始

```go
import "github.com/charlienet/gadget/cache"

// 仅本地内存缓存
c := cache.New(cache.WithMemStore())
c.Put(ctx, "key", "value", 60) // TTL 60秒
var v string
c.Get(ctx, "key", &v)

// 本地 + Redis 多级缓存
c = cache.New(
    cache.WithMemStore(),
    cache.WithStore(redisStore),  // 实现 cache.Store 接口的远程存储
)
```

## 核心 API

| 方法 | 说明 |
|------|------|
| `Get(ctx, key, &val)` | 从缓存获取值（local → remote），不存在返回 ErrEntityNotExist |
| `Getfn(ctx, key, &val, loadFn, expire)` | 缓存未命中时从数据源加载并缓存 |
| `Put(ctx, key, val, expire)` | 写入缓存（local + remote） |
| `Delete(ctx, keys...)` | 删除缓存并通知其他实例 |
| `PreLoad(ctx, loadFn, expire)` | 预加载批量数据 |
| `Update(ctx, key, updateFn)` | 更新数据后自动删除缓存（singleflight 保护） |
| `GetMulti(ctx, keys...)` | 批量获取 |
| `SetMulti(ctx, items, expire)` | 批量写入 |
| `Close()` | 关闭缓存（停止后台 goroutine） |

## 数据加载流程

```
Getfn(key, loadFn)
  │
  ├─ Local 命中? ───→ 返回
  │
  ├─ Remote 命中? ──→ 回写 Local → 返回
  │
  └─ 加载 loadFn(key)
       ├─ 存在 → 写入 Local + Remote → 返回
       └─ 不存在 → 缓存空值占位 → 返回 ErrEntityNotExist
```

## 缓存穿透防护

对于数据源不存在的 key，缓存一个空值占位符 `*`，后续请求直接从缓存返回 ErrEntityNotExist，不再穿透到数据源。

```go
err := c.Getfn(ctx, "not-exist", &v, loadFn, 60)
errors.Is(err, cache.ErrEntityNotExist) // true
```

## 并发控制 (Singleflight)

同一 key 的并发 `Getfn` 请求只放行一个去加载数据源，其余等待共享结果，防止缓存击穿。

## 跨实例失效通知

通过 Listener 机制（基于 Redis PubSub）实现多实例间的缓存失效同步：

```go
c := cache.New(
    cache.WithMemStore(),
    cache.WithListener(redisListener),
)

// 任意实例 Delete 后，其他实例自动清除本地缓存
c.Delete(ctx, "key")
```

## 存储选项

| Option | 说明 |
|--------|------|
| `WithMemStore()` | 堆内内存存储（支持 TTL、容量驱逐） |
| `WithStore(s)` | 自定义存储（实现 Store 接口） |
| `WithSerializer(s)` | 自定义序列化器（默认 JSON） |
| `WithTTL(seconds)` | 默认过期时间 |
| `WithName(name)` | 缓存实例名称（用于存储前缀） |
| `WithLogger(l)` | 日志记录器 |
| `WithTTLJitter(d)` | TTL 随机抖动（防缓存雪崩） |

### 缓存雪崩防护

#### TTL 随机化

同一批写入的 key 如果 TTL 完全一致，到期时会同时过期，造成数据库压力尖峰。
`WithTTLJitter` 在每个 key 写入时增加随机抖动：

```go
c := cache.New(
    cache.WithMemStore(),
    cache.WithTTLJitter(10*time.Second), // Put TTL 60 → 实际 60~70 秒随机
)
```

- `mem_store`：Put 时在 TTL 基础上加 [0, jitter) 随机纳秒数
- Redis 插件：通过 `WithTTLFactor(factor)` 加 [1, factor] 随机秒数

#### 空值缓存（缓存穿透防护）

数据源不存在的 key 自动缓存 `*` 占位符，后续请求直接返回 `ErrEntityNotExist`。

### 内存存储驱逐策略

```go
c := cache.New(
    cache.WithMemStore(),
    cache.WithMaxItems(10000),    // 最大条目数
    cache.WithMaxBytes(1<<20),    // 最大字节数（约1MB）
    cache.WithCleanupInterval(30*time.Second), // 后台清理间隔
)
```

- **TTL 过期清理**：后台协程定期扫描删除过期条目
- **FIFO 容量驱逐**：超出上限时按插入顺序驱逐最旧条目，已过期的优先清理
- **同步检查**：Put 时即时检查容量，无需等待后台周期

## 优雅降级

远程存储故障时自动降级为本地-only 模式，避免错误传播：

```go
c := cache.New(
    cache.WithMemStore(),
    cache.WithStore(redisStore),
    cache.WithDegradeThreshold(3),             // 连续失败3次进入降级
    cache.WithDegradeRecoveryInterval(10*time.Second), // 每10秒尝试恢复
)
```

- 降级期间跳过所有远程操作（Get、Put、Delete）
- 后台探活 goroutine 定期检查远程可用性
- 恢复后自动切回正常模式

## 批量操作

```go
// 批量读取
values, _ := c.GetMulti(ctx, "a", "b", "c")

// 批量写入
c.SetMulti(ctx, map[string]any{
    "x": "value1",
    "y": "value2",
}, 60)
```

## Metrics / 可观测性

```go
// 自定义 Metrics 采集（Prometheus / OTEL 等）
c := cache.New(
    cache.WithMetrics(myMetrics),
)
```

Metrics 接口：

```go
type Metrics interface {
    CacheEviction()           // 容量驱逐
    SetDegraded(on bool)      // 降级状态变化
}
```

## 统计信息

```go
st := c.Stats()
st.TotalHits()   // 总命中次数
st.TotalMiss()   // 总未命中次数
st.Total()       // 总查询次数
st.Clear()       // 重置计数
```

## 常见组合示例

### Local + Redis + 跨实例失效

```go
rdb := redis.New(...)
store := rediscache.New(rdb)
lis := redislistener.NewReidsListener(rdb, "cache:invalidate")

c := cache.New(
    cache.WithMemStore(),
    cache.WithStore(store),
    cache.WithListener(lis),
    cache.WithName("users"),
    cache.WithTTL(300),
)
```

### 最大 3 层：Local + Redis + 数据源

```go
c := cache.New(
    cache.WithMemStore(),
    cache.WithStore(redisStore),
)

// 自动穿透到数据源
var user User
err := c.Getfn(ctx, "user:123", &user, loadUserFromDB, 60)
```
