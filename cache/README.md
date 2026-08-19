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

## 部署模式

| 模式 | 配置 | 说明 |
|------|------|------|
| 只本地 | 零参 `New()` 或 `WithStore(本地 store)` | 默认注入内存缓存；单机场景 |
| 只远程 | `WithStore(redisStore)` 单独使用 | 不注入本地层（`localStore` 为 nil），所有读写直达远程；降级/versionSync 仍生效（remoteStore 存在时启动） |
| 两级（推荐） | `WithMemStore() + WithStore(redisStore)` | 本地 + 远程多级，兼顾性能与一致性 |

只远程模式下各读写路径对 `localStore == nil` 已有完整保护，可安全运行。

## 保护性能力（默认开启，零配置生效）

生产环境推荐的防雪崩 / 防穿透保护均已**默认生效**，无需显式配置；仅高级场景
（如 TTL 精确可控、禁用随机抖动）才需要显式关闭或调整：

| 能力 | 机制 | 默认 | 关闭 / 调整 |
|------|------|------|-------------|
| 防雪崩（L1 内存层） | 写入 TTL 叠加 0~30s 随机抖动 | 开启 | `WithTTLJitter(0)` 关闭；`WithTTLJitter(d)` 自定义范围 |
| 防雪崩（L2，redis 插件） | TTL 叠加随机秒数 | 开启 | `WithTTLFactor(0)` 关闭（见 redis 插件 README） |
| 防穿透（并发合并） | 同 key 并发请求 singleflight 合并，一个取其他等待 | 内建 | — |
| 防穿透（空值占位） | Getfn 回源未找到时缓存空占位拦截，防反复穿透数据源 | 内建 | — |
| 防穿透（自动降级） | 连续失败（默认 3 次）自动跳过 L2，恢复后补偿写/删 | 开启 | `WithDegradeThreshold` / `WithDegradeRecoveryInterval` 调整 |

防雪崩细节见下文「缓存雪崩防护」；redis 插件（L2 层）的随机抖动由其自身文档说明，本包只做总览引用。

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

## 启动预热（PreLoad）

全量加载常用需求：启动时用 `PreLoad` 从数据源一次性加载热点数据到缓存（内部
批量写入 `SetMulti`——一次序列化/加密/批量往返，优于逐 key `Put`）：

```go
c.PreLoad(ctx, func(ctx context.Context) (map[string]any, error) {
    // 从数据源全量加载热点数据
    return loadHotData(ctx)  // map[string]any
}, 60) // TTL 秒
```

与 `Getfn` 按需回源的分工：`PreLoad` 预热**热点数据**（启动期全量、批量）；
`Getfn` 处理**冷数据按需加载**（首次访问回源并缓存）。两者可组合：
启动预热热点 + 运行期按需回源，覆盖全部数据。

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

> **定位说明**：Redis PubSub/Stream 通知为**多节点兜底实现**（最多一次送达、
> 订阅时序窗口可能丢消息——就绪前发布的失效消息可能丢失，`Listener.Ready()`
> 可用于服务就绪等待）。生产环境建议使用 MQ（如 nats/kafka）实现 Listener：
> 实现 `cache.Listener` 接口即可接入，cache 包零改动。

### L1 本地插件选型

| 插件 | 特点 | 适合场景 |
|------|------|---------|
| `gcache` | 低并发 LRU、容量按条目数 | 小数据量、低并发、简单 LRU |
| `freecache` | 高并发、定长内存（容量按字节）、**per-key TTL 精确** | 需要 per-key TTL 的高并发场景 |
| `bigcache` | **全局 TTL（不支持 per-key）**、高并发、预分配内存 | TTL 全局统一、高吞吐场景 |

**一句话选择**：TTL 全局统一 → `bigcache`；需要 per-key TTL → `freecache`；小数据低并发 → `gcache`。

## 存储选项

### 基础选项（先跑起来）

| Option | 说明 |
|--------|------|
| `WithMemStore()` | 堆内内存存储（支持 TTL、容量驱逐）；零参 `New()` 默认注入 |
| `WithStore(s)` | 自定义存储（实现 Store 接口，可接 L1 插件或远程 L2） |
| `WithTTL(seconds)` | 默认过期时间（Getfn 传 0 时使用） |

### 进阶选项

| Option | 说明 |
|--------|------|
| `WithSerializer(s)` | 自定义序列化器（默认 JSON） |
| `WithName(name)` | 缓存实例名称（用于存储前缀） |
| `WithLogger(l)` | 日志记录器 |
| `WithTTLJitter(d)` | TTL 随机抖动范围（默认开启 0~30s；`WithTTLJitter(0)` 关闭） |

### 缓存雪崩防护

#### TTL 随机化

同一批写入的 key 如果 TTL 完全一致，到期时会同时过期，造成数据库压力尖峰。
TTL 随机抖动（jitter）**默认开启**：L1 内存层每个 key 写入时在 TTL 上叠加 0~30 秒随机值
（`defaultTTLJitter`，使用者零配置获得防雪崩保护）。

```go
c := cache.New(
    cache.WithMemStore(),
    // 默认已开启 0~30s 抖动（Put TTL 60 → 实际 60~90 秒随机），无需显式配置
)
```

- **关闭抖动**：`cache.WithTTLJitter(0)`（如需要精确 TTL 过期语义）
- **自定义范围**：`cache.WithTTLJitter(d)`（TTL 叠加 [0, d) 随机值）
- L2 层（Redis 插件）通过 `WithTTLFactor(factor)` 独立启用随机秒数——**两层各自默认开启、可分别关闭**

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

### 降级期间行为（使用端须知）

**触发与退出**：连续 `degradeThreshold` 次远程操作失败进入降级（`WithDegradeThreshold`，默认 3）；后台探测（`WithDegradeRecoveryInterval`）或任意远程操作成功时退出。日志信号：`entering degraded mode` / `exiting degraded mode`、`health probe failed/succeeded`。

**降级期间**：
- **Get/Getfn**：跳过远程查询——L1 miss 时 `Get` 返回 `ErrEntityNotExist`，`Getfn` 走 `fn` 回源（本地数据不受影响，不会被误判清除）；
- **Put**：仅写本地；远程写入缓冲到 pending，**缓冲满（1024 条）时返回 `ErrPendingWritesFull`**；
- **Delete**：**延迟生效**——远程删除进入 pendingDeletes，恢复后补偿执行（期间本地已删，防止数据复活）。

**恢复后**：pending 的写/删自动补偿回写远程；补偿失败保留待重试（日志 Warn：`flush pending write/delete ... failed`）。

**错误处理模板**（配合错误分类哨兵）：

```go
err := c.Get(ctx, key, &v)
switch {
case errors.Is(err, cache.ErrEntityNotExist):
    // 键不存在（含空值占位拦截）——正常业务路径
case errors.Is(err, cache.ErrRemoteUnavailable):
    // 远程存储故障——可返回兜底数据或走降级逻辑
case err != nil:
    // 其他错误（序列化、本地存储等）
}
```

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
