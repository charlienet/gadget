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
| `Invalidate(ctx, mutateFn)` | mutateFn 内写/删数据源并**返回受影响的全部缓存 key**（`func(ctx) ([]string, error)`），成功后库逐 key 失效：per-key singleflight 与回源串行（共享吞删时确定性补删）、本地+远程双删、listener 集群广播收敛；可选延时无条件补删兜底跨实例竞态（`WithDelayedSecondDelete`）。一次变更影响多 key 无需多次调用 |
| `DeletePattern(ctx, pattern)` | 按 glob 模式批量删除匹配键（不发送集群通知） |
| `Stats()` | 返回命中/未命中/回源等统计只读快照 |
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
| `WithCipher(c)` | 注入透明加解密器（默认不加密，详见"透明加解密 (Cipher)"） |
| `WithName(name)` | 缓存实例名称（用于存储前缀） |
| `WithLogger(l)` | 日志记录器 |
| `WithTTLJitter(d)` | TTL 随机抖动范围（默认开启 0~30s；`WithTTLJitter(0)` 关闭） |
| `WithHotKeyThreshold(n)` | L1 容量驱逐热 key 豁免：清理周期内命中 ≥ n 的条目优先跳过，单轮上限约 25%，热度窗口=清理周期，默认关闭（详见"内存存储驱逐策略"） |
| `WithDelayedSecondDelete(d)` | 延时无条件补删（默认 0 关闭）：`Invalidate` 首删后异步延迟 d 再做一次无条件删除并广播，兜底跨实例竞态回填的旧值；d 须大于业务最坏「回源读库+回填」耗时。代价：窗口内合法新写入一并被清除，下次回源自愈（fail-safe）；开启后每 key 每次失效正常产生 2 次广播，高频写场景 listener 流量约 ×2 |

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
    cache.WithCleanupInterval(30*time.Second), // 后台清理间隔（= 热度窗口）
    cache.WithHotKeyThreshold(100), // 周期内命中≥100 的 key 豁免容量驱逐（默认关闭）
)
```

L1 内存层为自研 map + 侵入式双向链表：

- **TTL 过期清理**：惰性（`Get` 命中即判断并摘除过期项）+ 后台协程按 `WithCleanupInterval`（默认 1 分钟）周期扫描清理
- **LRU 容量驱逐**：超出 `WithMaxItems`/`WithMaxBytes` 上限时，按"最近最少使用"从链表尾部逐出；读、覆写、`GetMulti`/`SetMulti` 命中都会把条目提到 MRU 端
- **热 key 豁免**（`WithHotKeyThreshold(n)`，`n<=0` 关闭）：在一个清理周期内命中 ≥ n 的条目，容量驱逐时优先跳过、转逐更冷的尾部条目。单轮最多跳过 `max(1, len(items)/4)`（约 25%）个热条目，达到预算后一律驱逐（降级），因此 **`len ≤ maxItems` 恒成立**、不会撑破容量；`maxItems=0` 仅设 `maxBytes` 时该预算同样生效。热度窗口 = `cleanupInterval`（每周期结束清零一次命中计数）
- **豁免只免容量驱逐，绝不免 TTL**：惰性过期、后台过期清理、`Delete`/`DeletePattern`、监听器失效、版本同步发现远程已删而清本地，全部无视热度照常生效
- **字节口径**：`usedBytes` 仅累计各条目 `len(值)`（不含 key 与结构开销），覆写精确扣旧加新
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

## 透明加解密 (Cipher)

库不内置加密实现（不绑定密钥来源与算法选型），但提供完整注入通道：应用实现
`Cipher` 接口并经 `WithCipher` 注入后，L1/L2 中存储的都是密文，`Put`/`Get`/
`Getfn`/`GetMulti`/`SetMulti` 全路径对调用方透明（明文进出）。

```go
type Cipher interface {
    Encrypt(plaintext []byte) ([]byte, error) // 序列化后的字节 → 密文
    Decrypt(ciphertext []byte) ([]byte, error) // 密文 → 序列化后的字节
}

c := cache.New(
    cache.WithMemStore(),
    cache.WithStore(redisStore),
    cache.WithCipher(myCipher), // nil 被忽略，默认不加密
)
```

### 格式与边界契约

| 契约 | 说明 |
|------|------|
| 存储布局 | `[9B 版本头] + [密文]`：版本头在 `Encrypt` 之后包裹、不参与加密，版本同步机制可在不解密的状态下比较时间戳 |
| 加密时机 | `Marshal → Encrypt → Store` / `Store → Decrypt → Unmarshal`，加密作用于序列化后的最终字节，L1/L2 统一存密文，不存在某层明文 |
| 空值占位符 | 防穿透占位符 `*` 不加密、明文存储（seal/unseal 双侧短路） |
| 并发性 | 同一 Cipher 实例可能被多 goroutine 并发调用，实现必须并发安全 |
| 失败语义 | 单个 key 解密失败返回 error（不影响同批其他 key），下一次回源自愈 |

### 参考实现：AES-GCM + 多密钥轮换（已验证）

密钥轮换通过"密文自带 1 字节 keyID"实现：`Encrypt` 恒用当前密钥，`Decrypt` 按
keyID 选钥，轮换期间新旧密钥并存，存量密文照常可读。以下实现已通过编译与
往返/轮换/篡改拒绝测试（`crypto/aes` + `cipher.GCM`，纯标准库，零新依赖）。

```go
// RotatingCipher 实现 cache.Cipher 接口：AES-GCM + 多密钥轮换。
// 密文格式：[1B keyID][12B nonce][GCM 密文+tag]。
type RotatingCipher struct {
    mu      sync.RWMutex
    current uint8
    keys    map[uint8][]byte // keyID → AES 密钥（16/24/32 字节）
}

// keyID 0 保留为"非本实现密文"哨兵，有效密钥 id 为 1~12。
func New(current uint8, keys map[uint8][]byte) (*RotatingCipher, error) {
    if current == 0 || current > 12 {
        return nil, fmt.Errorf("mycipher: current key id %d out of range 1..12", current)
    }
    if len(keys) == 0 {
        return nil, errors.New("mycipher: keys must not be empty")
    }
    for id, k := range keys {
        if id == 0 {
            return nil, errors.New("mycipher: key id 0 is reserved")
        }
        switch len(k) {
        case 16, 24, 32:
        default:
            return nil, fmt.Errorf("mycipher: invalid AES key length %d for id %d", len(k), id)
        }
    }
    if _, ok := keys[current]; !ok {
        return nil, fmt.Errorf("mycipher: current key id %d missing", current)
    }
    m := make(map[uint8][]byte, len(keys))
    for id, k := range keys {
        m[id] = append([]byte(nil), k...)
    }
    return &RotatingCipher{current: current, keys: m}, nil
}

// Rotate 切换 Encrypt 使用的密钥（新钥写入并设为当前）；旧钥保留仍可解密。
func (r *RotatingCipher) Rotate(newID uint8, newKey []byte) error {
    switch len(newKey) {
    case 16, 24, 32:
    default:
        return fmt.Errorf("mycipher: invalid AES key length %d", len(newKey))
    }
    r.mu.Lock()
    defer r.mu.Unlock()
    r.keys[newID] = append([]byte(nil), newKey...)
    r.current = newID
    return nil
}

// Retire 摘除旧钥。仅当该钥加密的所有缓存数据都已过期（等待时长 > 实例最大
// TTL）后才可调用，否则存量密文将永久解密失败。当前密钥不可摘除。
func (r *RotatingCipher) Retire(id uint8) {
    r.mu.Lock()
    defer r.mu.Unlock()
    if id != r.current {
        delete(r.keys, id)
    }
}

func (r *RotatingCipher) Encrypt(plain []byte) ([]byte, error) {
    r.mu.RLock()
    key, id := r.keys[r.current], r.current
    r.mu.RUnlock()

    gcm, err := newGCM(key)
    if err != nil {
        return nil, err
    }
    nonce := make([]byte, gcm.NonceSize()) // GCM 标准 12 字节随机 nonce
    if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
        return nil, fmt.Errorf("mycipher: read nonce: %w", err)
    }
    out := make([]byte, 0, 1+len(nonce)+len(plain)+gcm.Overhead())
    out = append(out, id)
    out = append(out, nonce...)
    return gcm.Seal(out, nonce, plain, nil), nil
}

func (r *RotatingCipher) Decrypt(data []byte) ([]byte, error) {
    if len(data) < 13 {
        return nil, errors.New("mycipher: ciphertext too short")
    }
    id := data[0]
    r.mu.RLock()
    key, ok := r.keys[id]
    r.mu.RUnlock()
    if !ok {
        return nil, fmt.Errorf("mycipher: unknown key id %d", id)
    }
    gcm, err := newGCM(key)
    if err != nil {
        return nil, err
    }
    ns := gcm.NonceSize()
    if len(data) < 1+ns+gcm.Overhead() {
        return nil, errors.New("mycipher: ciphertext truncated")
    }
    return gcm.Open(nil, data[1:1+ns], data[1+ns:], nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
    block, err := aes.NewCipher(key)
    if err != nil {
        return nil, err
    }
    return cipher.NewGCM(block)
}

// 编译期断言：满足 cache.Cipher 的形状。
var _ interface {
    Encrypt([]byte) ([]byte, error)
    Decrypt([]byte) ([]byte, error)
} = (*RotatingCipher)(nil)
```

### 运维纪律

- **轮换**：`Rotate(newID, newKey)` 后仅新写入用新钥；等所有旧钥密文随 TTL
  自然过期（等待时长 > 实例最大 TTL）再 `Retire(oldID)`。
- **密钥来源**：密钥材料由应用侧供给（环境变量 / KMS / 加密配置中心），
  严禁写入代码仓库；本参考实现只在内存中持有密钥副本（构造与 Rotate 时均
  做拷贝，防调用方外部改写）。
- **存量未加密数据的平滑启用**：注入 Cipher 后，Redis 中已存在的明文旧数据
  `Decrypt` 必然失败。推荐两种迁移策略，任选其一：
  1. **换前缀**（推荐）：启用加密时同步 `WithName("users-v2")` 切新 key 前缀，
     旧前缀数据靠 TTL 自然淘汰，零解密冲突；
  2. **清缓存**：低峰期对旧前缀执行批量删除，接受一波回源洪峰（已有
     singleflight + TTL 抖动兜底）。
  不建议在 `Decrypt` 里做"失败当明文返回"的兼容分支——认证加密的意义就在于
  拒绝任何不可信字节，静默降级会把密文损坏/被篡改也当成明文。
- **算法约束**：只应使用认证加密（AEAD，如 AES-GCM）；CBC 等无认证模式不
  提供完整性保障，勿用。

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
