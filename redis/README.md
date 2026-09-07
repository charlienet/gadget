# redis

键前缀

使用redis的hook机制完成统一的键前缀添加，使用指定的分隔符对键前缀进行分割


约束
rdb:=redis.New()
rdb.Constraint(Ping())


URL 连接三种运行模式

ParseURL/NewWithUrl 支持通过 URL 指定运行模式（纯解析，不连接服务器）：

- 单机（默认）：单地址
  ```go
  rdb, _ := redis.NewWithUrl("redis://:password@host:6379")
  ```
- 集群：逗号分隔多地址（种子列表），无 master_name；也支持官方 addr 参数追加地址
  ```go
  rdb, _ := redis.NewWithUrl("redis://:password@h1:7001,h2:7002,h3:7003")
  ```
- 哨兵：多地址（哨兵节点列表）+ master_name 参数
  ```go
  rdb, _ := redis.NewWithUrl("redis://:password@s1:26379,s2:26379,s3:26379?master_name=mymaster")
  ```

哨兵格式说明：go-redis 官方无哨兵 URL 格式，本库扩展了 master_name query 参数
（master_name 非空即创建 failover client）；其余官方参数（read_timeout、
addr 等）可混用，master_name 会被剥离后单独解析，不影响其他参数。
db path（如 /1）在集群/哨兵场景无意义（集群仅 db0），解析时忽略。


前缀 Hook（独立使用）

可在外部 go-redis client 上直接注册 PrefixHook，透明改写键前缀：

```go
rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
rdb.AddHook(redis.PrefixHook("myapp", ":"))
```

注意 Pub/Sub 边界：go-redis 的 SUBSCRIBE 走专用连接不经 hook，channel 前缀仅在
Publish 端生效；独立用法下订阅端请使用 SubscribeWithPrefix 保证两端对称：

```go
sub := redis.SubscribeWithPrefix(rdb, "myapp", ":", "events")
```


包装外部 client

用 NewWithClient 包装已有 go-redis client，获得本库 Client（前缀、约束、级联关闭等能力）。
注意：NewWithClient 不拥有传入的连接池，GracefulClose/Close 只关闭 AddPrefix 派生的子连接池，
外部 client 由调用方负责关闭：

```go
uc := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
rdb, err := redis.NewWithClient(uc, redis.WithPrefix("myapp"))
if err != nil { /* handle */ }
defer rdb.GracefulClose(context.Background()) // 只级联关闭派生池，不关闭 uc
defer uc.Close()                               // 外部自己关闭
```

AddPrefix 派生的子连接池自动继承 uc 的真实连接配置（地址、密码、DB、TLS 等），
无需重复传入连接 Option；显式传入的 Option 会覆盖。若包装的是无法提取配置的
自定义 UniversalClient，必须显式提供连接 Option（否则返回错误）。

需要完全手动控制连接配置（不走自动提取）时，用 WithRedisOptions 显式传入：

```go
uc := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", Password: "secret"})
rdb, err := redis.NewWithClient(uc,
    redis.WithPrefix("myapp"),
    redis.WithRedisOptions(redis.UniversalOptions{Addrs: []string{uc.Options().Addr}}),
)
if err != nil { /* handle */ }
```

WithRedisOptions 也可与 redis.New/NewWithUrl 配合，直接构造连接配置：

```go
rdb := redis.New(
    redis.WithRedisOptions(redis.UniversalOptions{Addrs: []string{"10.0.0.1:6379"}, Password: "secret"}),
    redis.WithPrefix("myapp"),
)
```


命令族前缀承诺边界

- 承诺：标准 KV/hash/set/zset/stream 键命令，以及已验证的模块命令
  （JSON.* / TS.* / BF.* / CMS.* / TDIGEST.* / FT.* 的键参数、EVAL/FCALL 的 KEYS 参数）。
- 不承诺：EVAL/FCALL 脚本内部硬编码的 key（脚本内字符串不受 hook 改写，需自行加前缀）；
  ACL / MEMORY / CLIENT / CONFIG 等诊断与权限命令（不涉及业务 key）；
  SORT 的 BY/GET pattern 外的边缘用法（pattern 内 `*` 通配的键模式已加前缀，
  不含 `*` 的 BY nosort 等除外）。
- 前缀幂等约定：传入命令的 key 不应自带前缀（前缀由 hook 统一添加）。
  手动拼接前缀后再传入会二次加前缀，属约定内的误用。
- 脚本内硬编码 key 不受前缀保护：使用 EVAL/FCALL 时，脚本里的 key 常量需在
  应用侧自行加上相同前缀，或改用 KEYS 参数传入。


AddPrefix 池语义

- 每前缀一个独立连接池：AddPrefix 派生新的 client，拥有独立连接池与更长的前缀。
- 父关闭级联子池：对父 client 调 GracefulClose/Close，会递归关闭所有派生子连接池。
- NewWithClient 场景：子池使用 uc 的真实连接配置（见上），且不关闭外部 uc。


cache 插件双层前缀示例

cache 插件 Initialize 时会对 store 再调一次 AddPrefix(opt.Name)，与 rdb 自身的
前缀叠加为双层前缀，例如：

```go
rdb := redis.New(redis.WithPrefix("app"))
store := cache_redis.New(rdb)                 // cache.Options{Name: "user"} 时
// 最终 key = "app" + ":" + "user" + ":" + key
```


MustConstraint 使用说明

MustConstraint 用于启动期强制校验（如版本、连通性），不满足时 panic 退出应用；
运行期的动态约束请使用 Constraint 并自行处理错误。


SubscribeWithPrefix

独立使用 PrefixHook 时，订阅端须用本函数显式加前缀（见上文 Pub/Sub 边界）。
本库 redisClient.Subscribe/PSubscribe/SSubscribe 已内置加前缀逻辑，无需使用本函数。


扩展能力

CAS 原子操作（Lua 比较并设置/删除）：

```go
ok, err := rdb.CompareAndSet(ctx, "k", "old", "new") // 值匹配才设置
ok, err = rdb.CompareAndDelete(ctx, "k", "old")      // 值匹配才删除
ok, err = rdb.CompareAndSet(ctx, "k", nil, "v")      // nil = key 不存在时设置（SETNX）
```

延迟队列（ZSET，Lua 原子取出）：

```go
q := rdb.NewDelayedQueue("task:delay")
q.Enqueue(ctx, payload, time.Now().Add(time.Minute)) // payload 支持 string 与 json 对象
payload, ok, err := q.Dequeue(ctx)                   // 原子取一个到期任务
items, err := q.DequeueBatch(ctx, 10)                // 原子批量取
```

### ⚠ 已弃用 / 迁移指引

本模块内嵌的 `RateLimiter`（令牌桶）与 `LeakyBucket`（漏桶）已弃用，新代码请优先使用独立模块：

- `github.com/charlienet/gadget/ratelimit` —— 限流器抽象（`ratelimit.New` + `WithRate`/`WithBurst` 实例级固定速率）
- `github.com/charlienet/gadget/plugins/ratelimit/redis` —— 其 Redis 后端（GCRA 批发脚本）

一句话差异：旧接口速率/突发为每次调用传参、结果用 `RateResult` 结构体表达；新模块速率在 Limiter 实例级固定、用 `(bool, error)` + `errors.As(*ExceededError)` 取 `RetryAfter`（不提供漏桶变体，恒定速率可用精确模式 `WithoutLocalLease` 近似）。以下示例保留供存量代码参考，不再演进。

限流器（按名称隔离 key 空间）：

```go
login := rdb.NewRateLimiter("login")
pay := rdb.NewRateLimiter("pay")
// 多实例/多模块部署时按名称隔离：login 与 pay 对相同业务 key 互不干扰
login.Allow(ctx, "user:1", 5)  // 最终 Redis key = 前缀 + "rate:login:user:1"
pay.Allow(ctx, "user:1", 10)   // 前缀 + "rate:pay:user:1"
// 空名称不隔离（行为与旧版一致）：rdb.NewRateLimiter("")
// Wait 阻塞模式（限速而非拒绝）：等待配额放行或 ctx 超时
if err := login.Wait(ctx, "user:1", 5); err != nil { /* 超时/取消 */ }
// AllowAtMost 尽力而为：配额不足时消耗剩余配额而非整批拒绝
res, _ := login.AllowAtMost(ctx, "user:1", 5, 10) // Consumed=实际消耗
// Reset 重置配额（运维解除限流/配置变更清状态）
if err := login.Reset(ctx, "user:1"); err != nil { /* ... */ }
```

漏桶限流（恒定输出速率、拒绝突发，与令牌桶互补）：

```go
lb := rdb.NewLeakyBucket("sms", redis.WithBurst(5))
res, err := lb.Allow(ctx, "user:1", 10) // 每秒恒定输出 10 个，超出桶容量的排队被拒
if !res.Allowed {
    // res.RetryAfter 为建议等待时长
}
// 阻塞等待版：lb.Wait(ctx, "user:1", 10) / lb.WaitN(ctx, key, n, per)
// 差异：令牌桶允许突发、按平均速率补令牌；漏桶输出速率严格恒定、拒绝突发。
```

布隆过滤器（双路径，无需 RedisBloom 模块）：

```go
bf := rdb.NewBloomFilter("bf:1", redis.WithCapacity(1000000), redis.WithFalsePositive(0.01))
added, err := bf.Add(ctx, "item1")            // 返回是否新增（已存在返回 false）
ok, err := bf.Exists(ctx, "item1")            // false=必不存在；true=可能存在
flags, err := bf.AddMulti(ctx, "a", "b", "c") // 批量，返回顺序与入参严格对应（对齐 BF.MADD）
info, err := bf.Info(ctx)                     // 元数据（bitmap 路径 NumItems 由 BITCOUNT 估算）
// 快捷等价：rdb.NewBloomFilterWithEstimate("bf:1", 1000000, 0.01)
```

分派逻辑：服务器加载了 RedisBloom 的 bf 模块 → 原生 BF.* 命令（自动扩容子
过滤器）；未加载 → 自动回退到 bitmap（GETBIT/SETBIT + Lua 原子脚本）实现
（普通 Redis 即可运行，无模块依赖）。回退版单次 EVAL 完成 k 位检查+置位
（并发 Add 同一 item 原子，恰一个返回"新增"）；AddMulti/ExistsMulti 走
**单次批量 Lua 脚本**（1 往返处理 n×k 个位，返回顺序与入参严格对应，对齐
BF.MADD 语义；不分块，超大 n 时单次脚本的 O(n·k) 服务端执行代价由调用方
控制批量大小）。可用 rdb.Capability().HasBloom() 预检模块是否加载。

Lua 能力记忆与降级兜底：EVAL 失败按错误类别三态记忆（连接级共享，
未知/支持/不支持）——仅"命令不存在/被禁"类（代理屏蔽、ACL 禁用等）
永久记住不支持、此后跳过 EVAL；服务瞬态错误（抖动/断连）不改写记忆、
按 FailPolicy 兜底；WRONGTYPE 等数据类错误原样透传。不支持 Lua 的服务器
自动降级为 **pipeline 非原子兜底**（k 个 GETBIT / SETBIT 各合并为 1 次
往返，Add 最坏 2k 次往返降为 2 次）；**该兜底路径不保证并发原子性**
（先查后写存在窗口，并发添加同一 item 可能都返回"新增"），属 Lua 不可用
时的尽力而为降级。

容量规划速查：最优位图 `m=ceil(-n·ln p/ln2²)`、哈希个数 `k=ceil(ln2·m/n)`、
每元素位数 ≈ `-log2(p)`（1% ≈ 9.6 bit、0.1% ≈ 10 bit）：

| 容量 n | 误判率 p | 位图 m | 内存（m/8） | k |
|---|---|---|---|---|
| 1 万 | 1% | 95,851 bit | ≈ 12 KB | 7 |
| 100 万 | 1% | 9,585,059 bit | ≈ 1.14 MB | 7 |
| 1000 万 | 0.1% | 143,775,876 bit | ≈ 17.1 MB | 10 |
| ≈4.5 亿 | 1% | 2^32-1 bit | 512 MB（位图上限） | 7 |

容量契约声明：bitmap 路径容量**创建时固定、位图不扩容**；插入超过预估容量
后误判率按 `(1-e^(-k·n'/m))^k` 单调恶化且**不可恢复**（位图无删除语义），
属应用端容量规划责任。解法：预估充足容量 / 周期性重建（换新 key 灌入）/
部署 RedisBloom 模块（BF.* 路径自动扩容）。非法参数静默回落默认值：
`WithCapacity(n<=0)` 保留 1000000、`WithFalsePositive` 仅接受 (0,1) 开区间。

上限与成本：位图上限 2^32-1 bit（Redis 字符串 512MB 限制），p=0.01 时
capacity 超约 4.5 亿将在创建时 fail-fast panic（提示降低 capacity 或部署
RedisBloom）；`Info` 的 NumItems 由 BITCOUNT 置位数反推
（`numItems≈-(m/k)·ln(1-bitsSet/m)`），BITCOUNT 为 O(bytes) 全量扫描，
仅适合低频运维查询。

**Redis Cluster 分片（显式 opt-in）**：分片默认关闭——v0.5.0 起须显式
`WithShardCount(n>1)` 才启用。`Mode()==ModeCluster` 且 `n>1` 时工厂把过滤器
打散为 `effectiveN` 个物理键 `<base>#<idx>`（idx = xxh3-128 高 64 位 %
effectiveN，分片键不带 hash tag、由 go-redis 按整键自动路由），突破单键
单节点的容量与并发瓶颈；standalone/sentinel/ring、或集群未显式开启（默认
n=1）时不分片，键名与行为完全不变。`WithCapacity` 在分片下是**全局总容量**，
均摊每分片（`ceil(总/N)`）且下限 1000——容量不足时分片数自动收缩
（`effectiveN = min(请求 N, 总/1000)`，总容量 <2000 退化为单片）；
`WithShardCount(n)` 设置请求分片数（**默认 1 即关闭**，n<=0 静默忽略保留
默认 1，仅 ModeCluster 生效）。集群下开启后即使退化为单片也统一带 `#0`
后缀（键名连续）。语义注意：

- 分片后实测 FPR ≈p 且**略偏高**（哈希到各分片的负载天然不均，每分片
  容量越小越明显），对误判率敏感的场景预留余量或增大总容量。
- base 键自带 `{hashtag}` 时所有分片键路由同一 slot，分片退化为纯命名
  拆分（行为仍正确，但失去跨节点打散的意义）。
- 开启分片（`WithShardCount(n>1)`）与未分片（默认/standalone）**键空间
  不互通**：前者键名带 `#idx` 后缀，切换部署形态或分片配置会读不到旧键，
  等同重建过滤器（布隆无删除语义，只能重新灌入），请纳入迁移预案。
- 集群下 `HasBloom()`（路径分派依据）与 Lua 能力记忆只探测 go-redis
  路由到的**随机单节点**，要求各节点模块/配置同构，否则分派错路径会
  表现为部分分片键 "unknown command" 类错误。
- `Info()` 对每个分片键各发一轮命令（成本 ×effectiveN），更严格限制为
  低频运维查询；`AddMulti`/`ExistsMulti` 中途失败时可能已部分写入
  （已成功的分片不回滚）——布隆置位幂等、整体重试无数据危害（仅重试
  时"新增"返回值失准）；任一分片组服务不可用则全部结果按 FailPolicy
   整体兜底（FailOpen 全 true / FailClosed 全 false + 哨兵错误），不产生
   混合结果。
- 分片批量路径（`multiSharded`）在**单个 Pipeline 内发送 inline EVAL 而非
  EVALSHA**：Pipeline 内无法表达 NOSCRIPT 重试——EVALSHA 的错误要 `Exec`
  后才可见，同一 Pipeline 不能补发 EVAL（go-redis `Script.Run` 的 NOSCRIPT
  回退只覆盖非 pipeline 单命令）。脚本体仅数百字节，内联的额外带宽开销可
     忽略，是"单 Pipeline"约束下的最优实现；单键路径仍走 EVALSHA+三态记忆。

**性能特征（2026-09 压测，50 并发×10s、容量 200k、单机 3 主 3 从，强制双路径 bf/bitmap A/B）**：

- 单条 Add/Exists：分片无实质代价——各分片数结果均落在 ±10% 噪声带内；
  服务端 `bf.add`≈3µs、`bf.exists`≈2µs，ClusterClient 相比 standalone 的固有
  路由开销约 5%。
- 批量 AddMulti/ExistsMulti：一批散到 ≤N 组即 ≤N 条命令，掉幅随分片数单调
  上升——8 分片 bf addmulti −44.7% / bitmap existsmulti −47.5%，4 分片
  −25~−40%，2 分片 −12~−18%（区间不含 bitmap addmulti 的 −28.5%——该测点
  与 1 分片同代码路径，系漂移嫌疑值已排除），1 分片 ≈standalone（P99 长尾系单机 7 实例抢
  CPU，多机部署预期好转；命令数 ×N 的税与机器数无关）。
- 选型建议：读多写少、以单条预筛为主可随意分片；高频批量场景不开分片或 n
  取小；有 RedisBloom 优先 BF.\* 路径（约 2× QPS、自动扩容、无容量悬崖）；
  批量的甜蜜区是单命令 `BF.MEXISTS`/`BF.ADD` 形态（把分组散列交回服务端）。
- 路由实现：分片路由 `idx = xxh3.Hash128(item).Hi % n` 约 3.9ns/item，相对
  网络往返可忽略、非成本项；曾评估"取首字节做模"的进一步优化被否决——业务
  键常共享前缀，首字节取模会把同前缀键全打到单一分片、打穿负载分布，纯负
  收益。

**强制实现路径 `WithBloomImpl`**：默认 `BloomImplAuto` 按 `HasBloom()`
探测选 BF.*/bitmap；`BloomImplBF` / `BloomImplBitmap` 跳过能力探测直接
选定实现，用途是同一实例上对两条路径做 **A/B 对照测试**（不同 key 各
强制一路）。"强制"即字面义：无模块服务器上 `BloomImplBF` 的命令直接
报 "unknown command" 类错误且**不自动降级**；`BloomImplBitmap` 无视模块
恒走 Lua/pipeline 路径。越界枚举值按 auto 处理。生产建议保留 auto，让
包装层自动利用模块能力。两条路径的分片路由/分组回填/Info 聚合/FailOpen
语义完全一致（共享同一实现层），强制选项只影响命令层选择。

**BREAKING（v0.5.0）**：位图路径换 xxh3-128 双哈希，并奇化位图大小（m|1）
与哈希步长（h2|1）——修复旧实现步长与模数共享公因子 2 导致的探测轨道减半
（实测 p=0.001 时 FPR 超标 34.95 倍）。旧位图 key 对新代码会产生**假阴性**，
升级时必须删除旧 key 或换 key 重建；BF.*（RedisBloom）路径不受影响，
 并修正 bitmap 路径 Add 新增判定（对齐 BF.ADD：调用前不可能存在即返回 true）。

**BREAKING（v0.5.0）测试辅助与环境变量**：`redis/test` 包删除 `RunOnRedisStack()`（改用 `RunOnRedis()`）、`RunOnMiniRedis()`（改用 `mini.Run()`，import `github.com/charlienet/gadget/redis/test/mini`）。环境变量对照迁移：

| 旧 | 新 |
|---|---|
| `REDIS_STACK_URL` | 并入 `REDIS_URL` |
| `REDIS_CLUSTER_ADDRS` + `REDIS_PASSWORD` | 统一为 `REDIS_CLUSTER`（完整 URL，密码内嵌） |

`REDIS_CLUSTER` 为完整 URL 格式（密码写在 userinfo）：`redis://:pass@host1:7001,host2:7002`；单机/哨兵的 `REDIS_URL` 同形式，如 `redis://:pass@host:6379`。


 布谷鸟过滤器（双实现，无需 RedisBloom 模块）：

```go
cf := rdb.NewCuckooFilter("cf:1", redis.WithCuckooCapacity(1000000))
cf.Add(ctx, "item1")      // 返回是否新增（已存在返回 false）
cf.Exists(ctx, "item1")   // 存在性检查
cf.Del(ctx, "item1")      // 支持删除（与 BF 不同）
cf.Info(ctx)              // 元数据
```

分派逻辑：服务器加载了 RedisBloom 的 cuckoo 模块 → 原生 CF.* 命令；
未加载 → 自动回退到 Hash + Lua 实现（普通 Redis 即可运行，无模块依赖）。
回退版特征：单次往返（每条操作一条 Lua 脚本）、状态存于单个 Hash key
（field=桶索引，value=桶内指纹数组）、模加候选桶 + 方向位驱逐链保证
**插入成功的元素必可命中（无假阴性）**；驱逐置换与 CF.ADD 语义对齐
（超容量时 Add 返回 false，元素可能被驱逐丢失，属 cuckoo 正常行为）。
可用 rdb.Capability().HasCuckoo() 预检模块是否加载。


go-redis 升级回归提醒

升级 go-redis 时请验证：集群重定向（MOVED/ASK）后前缀改写仍正确、
命令重试（MaxRetries）不导致前缀二次添加、EVAL/FCALL 的 KEYS 参数
改写不受脚本引擎变化影响。本地无法模拟，需真实 Redis 集群环境。


Redis 失效兜底（fail-open / fail-closed）

Redis 服务不可用时（dial 失败、读写超时、连接池超时、连接关闭等连接/服务层
故障），各扩展按 FailPolicy 策略放行或拒绝，避免业务直接挂掉：

| 扩展 | 默认策略 | 兜底语义 |
|---|---|---|
| 限流（RateLimiter/LeakyBucket） | FailOpen | Allow 返回放行、Wait 直接放行（保护性能力，宁可多放） |
| 布隆/布谷鸟过滤器 | FailOpen | Add/Exists 返回成功值（防穿透失效但放行业务） |
| CAS / 延迟队列 | 无策略 | 写操作不可降级，直接返回原始错误 |

显式配置（各扩展 Option 通过 WithFailPolicy 泛型设置）：

```go
rl := rdb.NewRateLimiter("login", redis.WithFailPolicy[*redis.RateLimiter](redis.FailOpen))
cf := rdb.NewCuckooFilter("cf:1", redis.WithFailPolicy[*redis.CuckooConfig](redis.FailOpen))
```

边界与可观测性：
- 兜底只覆盖**连接/服务不可用类错误**；命令级错误（WRONGTYPE、语法错误等）
  必须照常返回，不兜底吞掉。
- 调用方 ctx 取消/超时不触发兜底（主动行为 ≠ Redis 失效）。
- 兜底生效时返回**兜底值 + ErrRedisUnavailable 哨兵错误**（包装原始错误，
  `errors.Is(err, redis.ErrRedisUnavailable)` 可判断脱机事件，应用层自行
  处理告警/降级）：
  ```go
  ok, err := rl.Allow(ctx, "user:1", 5)
  if errors.Is(err, redis.ErrRedisUnavailable) {
      // Redis 不可用，扩展已按策略兜底
      notifyAlert() // 自行处理：告警/降级
  }
  ```
- FailClosed 的阻塞/循环语义不会死循环：Wait 失效返回错误。

自动重连

Redis 宕机恢复后 client 自动重连，无需额外配置——这是 go-redis 连接池的
固有行为：请求时获取连接、失败时重新 dial 并丢弃坏连接；宕机期间操作报
连接错误（可被 IsUnavailable 判定触发兜底），恢复后新请求自动重建连接
（集群 client 还会自动刷新拓扑）。测试 TestAutoReconnect 以 miniredis
固定端口模拟"宕机→恢复"验证了该行为。


熔断器（circuit breaker，默认启用）

Redis 服务失效后避免每次请求都等待连接超时：连续失败达阈值进入 Open
状态**快速失败**（不实际连接），冷却后半开放行探测请求，成功自动恢复。
状态机实现由 [gadget/breaker](https://pkg.go.dev/github.com/charlienet/gadget/breaker)
提供（本模块的 `CircuitBreaker` 为其转发 wrapper，仅 Classifier 注入
`IsUnavailable` 并保留 go-redis hook 适配）。

- 三态：Closed（正常）→ Open（连续失败 ≥ 阈值，快速失败）→ HalfOpen
  （冷却后放行单个探测，单飞：并发下同时只放行一个）→ 成功回 Closed。
- 默认参数：阈值 3 次（仅连接类错误计数，命令级错误不计入）、冷却 1s
  （短冷却保证快速重连探测）。配置：

  ```go
  rdb := redis.New(
      redis.WithAddr("127.0.0.1:6379"),
      redis.WithBreakerThreshold(5),               // 连续失败阈值
      redis.WithBreakerCooldown(500*time.Millisecond), // 冷却期
      // redis.WithBreaker(false)                  // 显式关闭
  )
  ```

- 与兜底联动：熔断 Open 快速失败返回的连接类错误同样被扩展层
  IsUnavailable 识别并走 FailPolicy 兜底（errors.Is(ErrRedisUnavailable)
  命中）；半开探测成功即自动闭合恢复，无需人工干预。
