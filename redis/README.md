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
card, err := bf.Card(ctx)                     // 去重基数估计（对齐 BF.CARD，与 NumItems 口径不同，见下）
err = bf.Reset(ctx)                           // 就地清空复用实例（容量/FPR 约束不变）
// 快捷等价：rdb.NewBloomFilterWithEstimate("bf:1", 1000000, 0.01)
```

分派逻辑：服务器加载了 RedisBloom 的 bf 模块 → 原生 BF.* 命令（自动扩容子
过滤器）；未加载 → 自动回退到 bitmap（GETBIT/SETBIT + Lua 原子脚本）实现
（普通 Redis 即可运行，无模块依赖）。回退版单次 EVAL 完成 k 位检查+置位
（并发 Add 同一 item 原子，恰一个返回"新增"）；AddMulti/ExistsMulti 走
**单次批量 Lua 脚本**（1 往返处理 n×k 个位，返回顺序与入参严格对应，对齐
BF.MADD 语义；不分块，超大 n 时单次脚本的 O(n·k) 服务端执行代价由调用方
控制批量大小）。可用 rdb.Capability().HasBloom() 预检模块是否加载。

**item 参数类型（any）与序列化冻结契约**：`Add/Exists/AddMulti/ExistsMulti`
的 item 是 `any`，位哈希与集群分片路由前统一编码为规范字节。支持的类型
白名单（与 go-redis `internal/proto/writer.go` 逐类型一致）：

| 类型 | 编码字节 |
|---|---|
| `nil` | 空 |
| `string` / `[]byte` | 原样字节 |
| `int`/`int8`/`int16`/`int32`/`int64`、`uint`/`uint8`/`uint16`/`uint32`/`uint64` | 十进制文本（`-42`、`255`） |
| `float32` / `float64` | 最短十进制浮点（`float64(1.0)`→`1`；float32 先转 float64 再格式化） |
| `bool` | `"1"` / `"0"`（**不是** `true`/`false` 文本） |
| `time.Time` | RFC3339Nano 文本 |
| `time.Duration` | 纳秒整数文本（`time.Second`→`1000000000`） |
| `net.IP` | 原始字节（4 或 16 字节 v4-in-v6；不文本化、不做 To4 归一） |
| `encoding.BinaryMarshaler` | `MarshalBinary()` 结果原样透传 |

其余类型——含**指针变体**（`*string`、`*int` 等，与 go-redis writer 的有意
差异）、struct、map、slice——返回数据类错误
`redis: can't marshal %T (implement encoding.BinaryMarshaler)`：不 panic、
不触发 FailPolicy 兜底、不发命令。

该编码格式**冻结**：BF.*/CF.* 路径由 go-redis 序列化原始 item 发往服务端，
bitmap 与 Lua cuckoo 回退路径由客户端 `marshalItem` 编码后哈希，两者必须
同字节，否则同一 item 会随实现路径落到不同的位/分片。回归防线
`marshal_writer_parity_test.go` 把每个金表值经真实 go-redis 链路写入
miniredis 再取回逐字节对照，go-redis 升级导致格式漂移时该测试必红。
**存量过滤器数据的有效性依赖此格式永久不变**——任何改变编码结果的调整都属
BREAKING，须升主版本并附"重建过滤器"迁移方案。

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
属应用端容量规划责任。解法：预估充足容量 / 周期性重建（换新 key 灌入，或
`Reset` 就地清空复用同一实例，见下文 Reset 说明）/ 部署 RedisBloom 模块
（BF.* 路径自动扩容）。非法参数静默回落默认值：
`WithCapacity(n<=0)` 保留 1000000、`WithFalsePositive` 仅接受 (0,1) 开区间。
本库不封装 BF.INSERT/CF.INSERT（含 autocreate 选项）：预分配一律经
WithCapacity/WithEstimate/WithCuckooCapacity 惰性 RESERVE；回退路径首写
天然 autocreate。

上限与成本：位图上限 2^32-1 bit（Redis 字符串 512MB 限制），p=0.01 时
capacity 超约 4.5 亿将在创建时 fail-fast panic（提示降低 capacity 或部署
RedisBloom）；`Info` 的 NumItems 由 BITCOUNT 置位数反推
（`numItems≈-(m/k)·ln(1-bitsSet/m)`），BITCOUNT 为 O(bytes) 全量扫描，
仅适合低频运维查询。

**Card（去重基数）与 Info().NumItems 的区别**：`Card` 返回过滤器中**不同
元素**数量的估计（去重口径，对齐 BF.CARD 语义）；`Info().NumItems` 是
**插入口径**——BF.* 路径含重复插入计数，同一元素 Add 两次 NumItems 加 2、
Card 不变。两路径的估计器不同：BF.* 路径为模块内概率基数计数器，bitmap
路径由置位数反推（与 Info.NumItems 同源估计量，分片下对每个分片独立估算
再求和）；**误差不承诺统一上界**，仅作观测，勿做精确业务计数。分片模式
下 Card 为全分片求和；键不存在返回 0；bitmap 路径位图饱和时钳制到容量
上界（此时估计量已失效，见容量契约声明）。成本与 `Info` 同级重命令
（分片 ×N 往返 / BITCOUNT 全量扫描），**勿入热路径**。服务不可用时返回
`(0, ErrRedisUnavailable 哨兵)`（`errors.Is` 可感知），**不随 FailPolicy
分叉**——观测类方法没有"放行/拒绝"概念；数据类错误原样返回。

**Reset（就地清空）**：删除全部物理键、计数归零，实例约束（容量/FPR）
不变——与"换新 key 重建"并列的容量超容对策，省去换 key 灰度切换的接线
成本。语义与限制：单键形态（未分片）为一条 DEL，Redis 单命令**原子**；
集群分片下逐分片 DEL 跨 slot **无法原子**，返回错误时可能只清空部分分片
——DEL 幂等，可安全重试直至成功；键不存在时返回 nil（幂等）。失败
**恒返回错误**（Unavailable 类包装为 `ErrRedisUnavailable` 哨兵，
`errors.Is` 可感知），与 FailPolicy 取值无关——"没清掉却假装清了"不可
接受。与并发 Add/Exists 无全序保证，需要强一致清空的场景请改用全新键 +
指针原子替换。模块路径（BF.\*）会同步复位惰性 BF.RESERVE 闸门，后续首次
写入重新按配置 RESERVE；**绕开 Reset 手搓 DEL 后复用同一实例，键会被
RedisBloom 默认参数（capacity=100）静默重建，容量契约作废——清空请一律
走 Reset**。Reset 删除整个键，勿与其他数据共用该键。

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
- 集群下 `HasBloom()`（路径分派依据）、`HasCuckoo()`/`HasCMS()` 等命令族
  判定（`COMMAND INFO` 探测）与 Lua 能力记忆都只探测 go-redis 路由到的
  **随机单节点**（`COMMAND INFO` 是无 key 命令，集群下不做逐分片探测），
  要求各节点模块/配置同构，否则分派错路径会表现为部分分片键
  "unknown command" 类错误。
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

**实现路径选择**：实现路径恒由 `HasBloom()` 能力探测自动选择（有 bf 模块
走 BF.\*、无则 bitmap 回退），**不提供强制旋钮**——两路径数据布局不互通，
任何路径切换本就等同重建过滤器；需要双路径对照时用两个不同 key 分别灌入
同批数据。

**BF.\* standalone 容量提示**：仅集群分片模式下 BF.\* 路径会按配置容量惰性
`BF.RESERVE` 预分配；单机（不分片）BF.\* 首写按 RedisBloom 默认
capacity=100 autocreate 后进扩容链，大规模插入时实测 FPR 仍贴预算线
（实测灌入 10k、设定 0.01：FPR≈0.0099，未超预算；bitmap 回退路径同参数
实测 0.0000）；对 FPR 敏感或需余量的场景建议分片（每分片 RESERVE 生效）
或预留更大容量。

**减少 Redis 请求数的推荐姿势（布隆/布谷鸟通用）**：

- **合批优先**：单条 `Add`/`Exists` 的延迟主要来自一次网络 RTT（内网典型
  0.2–0.4ms/次，Redis 服务端执行仅 µs 级）；改用 `ExistsMulti`/`AddMulti`
  批量接口后成本摊到每 item 约 2–4µs（实测 CF 路径 1000 项 1.8µs/item），
  高 QPS 判定场景优先攒批。
- **确需缓存否定结论时，用应用层有界 TTL 负缓存**（秒级窗口自愈），
  窗口内的重复查询不再打 Redis。
- ⚠ **不要在无失效通道的前提下用本地无界结构（如进程内 bloom 镜像）
  缓存"不存在"结论**：多实例共享同一 filter 键时，其他写入方的元素会被
  本地镜像永久静默误判为不存在。本库不提供此类 L1 镜像。

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

**BREAKING（v0.7.0）过滤器 item 参数 string→any**：`BloomFilter` 的
`Add/Exists/AddMulti/ExistsMulti` 与 `CuckooFilter` 的 `Add/Exists/Del`
参数类型由 `string` 改为 `any`（支持类型清单与序列化冻结契约见上文
布隆章节）。对**调用方兼容**——原有 string 实参无需任何改动，且同一
string/[]byte 值的哈希结果与升级前逐位一致；对**自行实现接口的调用方
（测试 mock、装饰器）是破坏性变更**，须同步把方法签名改为 `item any` /
`items ...any`。不支持的类型返回数据类错误（`redis: can't marshal ...`）
而非 panic，且不落入 FailPolicy 兜底判定。

**BREAKING（v0.8.0）布隆过滤器接口与选项变更**（三条，按 v0.x 惯例以
minor 承载）：

1. `BloomFilter` 接口新增 `Reset(ctx) error` 与 `Card(ctx) (int64, error)`
   方法——对外部**自行实现 `BloomFilter` 接口的调用方**（测试 mock、
   装饰器）是 source-breaking，须补齐两个方法；库内置实现（BF.\* 与
   bitmap 两路径）已实现。新能力语义（Reset 的集群分片非原子/幂等约束、
   Card 的去重估计口径与成本警告）详见各方法 godoc。
2. 移除 `WithBloomImpl` Option 与 `BloomImpl` 枚举，**无 Deprecated
   过渡**。迁移：从构造参数中删除该 Option 即自动回落 `HasBloom()`
   探测分派，其余参数与行为不变；双路径对照用两个不同 key（两路径数据
   布局本不互通，移除不引入额外数据迁移）。
3. 语义口径两处：`CuckooFilter.Add` 的两路径分叉（模块版 CF.\* 为多重集
   语义、重复插入可累积计数；回退版 hash 实现为去重语义）属**文档澄清**，
   无行为变化；`Del` 对不存在键的返回值做了**跨路径行为对齐**（归一化，
   门面统一返回 `(false, nil)`）——此前依赖模块路径错误形态的调用方，
   请改判 `(false, nil)`。


 布谷鸟过滤器（双实现，无需 RedisBloom 模块）：

```go
cf := rdb.NewCuckooFilter("cf:1", redis.WithCuckooCapacity(1000000))
cf.Add(ctx, "item1")                  // 返回是否插入（模块版多重集/回退版去重，见下方语义矩阵）
cf.Exists(ctx, "item1")               // 存在性检查（无假阴性）
cf.ExistsMulti(ctx, "a", "b", "c")    // 批量存在性，单命令往返、结果顺序与入参对应
cf.AddNX(ctx, "item1")                // 存在即不加（需要唯一性保证一律用它）
cf.Count(ctx, "item1")                // 出现次数估计（0=确定不存在）
cf.AddMulti(ctx, "a", "b", "c")       // 批量添加（对齐 CF.INSERT 语义）
cf.Del(ctx, "item1")                  // 支持删除（与 BF 不同）
cf.Info(ctx)                          // 元数据
cf.Reset(ctx)                         // 整键就地清空（幂等、可重试）
```

分派逻辑：服务器加载了 RedisBloom 的 cuckoo 模块 → 原生 CF.* 命令；
未加载 → 自动回退到 Hash + Lua 实现（普通 Redis 即可运行，无模块依赖）。
回退版特征：单次往返（每条操作一条 Lua 脚本）、状态存于单个 Hash key
（field=桶索引，value=桶内指纹数组）、模加候选桶 + 方向位驱逐链保证
**插入成功的元素必可命中（无假阴性）**；驱逐置换与 CF.ADD 语义对齐
（超容量时 Add 返回 false，元素可能被驱逐丢失，属 cuckoo 正常行为）。
可用 rdb.Capability().HasCuckoo() 预检 CF.* 命令族是否可用（v0.7.0 起经
`COMMAND INFO CF.ADD` 真实确认，不再是"查模块名"）。item 参数同样是 `any`，
支持类型清单与序列化冻结契约见上文布隆章节（回退版的指纹/桶索引由
`marshalItem(item)` 的规范字节导出）。

**Add / AddNX / AddMulti / Count 语义矩阵**：两路径语义不一致，跨路径
部署勿依赖单一口径：

| 方法 | 模块版（CF.*） | 回退版（Hash + Lua） | 适用场景 |
|---|---|---|---|
| Add | **多重集插入**：已存在也会再插一份；false 源于桶满/驱逐超限，非"已存在"信号 | 去重式：false 即已存在 | 仅当明确需要多重集计数；不跨路径依赖重复 Add 增值 |
| AddNX | 存在即不加（CF.ADDNX）；false 原因不承诺可区分（已存在或桶满/驱逐超限） | 与 Add 等价（脚本本就是 NX 语义，显式别名） | **需要唯一性/去重保证一律用本方法** |
| AddMulti | 每元素多重集插入，结果与入参顺序一一对应（对齐 CF.INSERT） | 每元素去重新增，结果与入参顺序对应 | 批量写入；见下方重试警告 |
| Count | 出现次数估计，可取任意值（指纹碰撞会高估） | 恒 0/1（Add 去重语义；同指纹碰撞的不同元素也计入，为高估来源） | 计数观测：0=确定不存在（无假阴性），>0 为估计 |

**⚠ AddMulti 重试警告**：失败时可能已部分写入；模块版重试会把已插入项
再插一份（多重集）——**Count 计数翻倍、返回值非终态**，重试前先确认可
接受重复插入。需要幂等回填的场景：先 `ExistsMulti` 去重再批量补写，或
逐条 `AddNX`。回退版为去重 NX 语义，整条 Lua 在 Redis 侧原子，重试同批
不会重复增值。

批量攒批与负缓存的推荐姿势（布隆/布谷鸟通用）见布隆章"减少 Redis 请求数
的推荐姿势"。

底层实现口径：模块版 AddMulti 为**单条 CF.INSERT**，不带 CAPACITY/
NOCREATE 选项——预分配统一走 WithCuckooCapacity 的惰性 CF.RESERVE 通道
（避免 RESERVE 与 INSERT 双通道配置语义分裂）；回退版为**单条批量 Lua**
（原子），单次执行时长 O(n×maxIterations×bucketSize)，超大批量会阻塞
Redis，由调用方控批、本库不分块。`Info` 回退版仅 **Size/NumBuckets/
NumItems/BucketSize 四字段有效**（Size 为估算：占用桶×桶字节数，
NumBuckets 为占用桶数），其余 NumFilters/NumDeletes/Expansion/
MaxIterations 恒 0；模块版全字段有效。

**Reset（就地清空）**：整键销毁、重建过滤器。与布隆章同族语义但更简单：
两路径状态都在单个键内（CF.* 键 / 单个 Hash），一条 DEL **天然完全
原子**——不存在 bloom 分片形态"部分清空可见"的问题；键不存在时返回
nil（幂等，可安全重复/失败重试）；不承诺与并发写入的相对次序（并发
Add 的元素可能落在清空前后两个世代）。与 Del 的边界：Del 删除单个已知
原文的条目（**需持有 item 本体**，且过滤器无枚举能力，无法用于全清），
Reset 整键销毁。Reset **≠ CF.COMPACT**：后者是 RedisBloom 的内部整理
命令、数据保留（本库未封装 CF.COMPACT，Reset 不隐式调用它）。模块版
会同步复位惰性 CF.RESERVE 闸门，后续 Add 按 WithCuckooCapacity 等配置
重建；绕开 Reset 手搓 DEL 后复用实例，配置会被模块默认参数静默替换
（纪律与布隆章相同——清空一律走 Reset）。失败**恒返回错误**（Unavailable
类包装为 ErrRedisUnavailable 哨兵，`errors.Is` 可感知），不受 FailPolicy
兜底影响——"没清掉却假装清了"会让会话隔离静默失效。

**BREAKING（v0.7.0）回退版主哈希换源 fnv1a→xxh3-64**：Lua 回退实现
（hashImpl）的指纹 `fp` 与候选桶 `i1` 的主哈希由 `fnv1a`（FNV-1a 64 位）换
为 `xxh3.Hash`（xxh3-64）；`i2` 的派生公式（乘法哈希 + 模加）与 CF.*
（RedisBloom 模块）路径均不受影响。动机有二：xxh3 吞吐显著高于 FNV-1a；
FNV-1a 低位雪崩质量差——`i1 = h % numBuckets` 在 numBuckets 为 2 的幂时
只取低位、桶分布偏斜，`fp = h & 0xFFFF` 同样受低位相关性影响而聚集，
xxh3-64 全位雪崩均匀修正该偏斜。

代价是**存量数据不可读**：旧哈希写入的回退版过滤器在新版本下条目定位口径
变了，表现为 `Exists` 假 miss、`Del` 失效（不是误判率变化，而是查错桶）。
**升级后请重建存量 Lua cuckoo 过滤器**：`Del` 旧 key 后重新灌入，或直接换
新 key 灰度切换。受影响面：凡走过回退版（Lua 键结构）的过滤器都需重建
——服务器确实没有 CF.* 的，因本次哈希换源重建；服务器本来有 CF.* 的，因
能力探测修复（见下）会自动改分派 CF.*，旧回退键结构不再被读。原本就由
CF.*（模块版）写入的存量数据不受影响。

**BREAKING（v0.7.0）能力探测修复：HasCuckoo/HasCMS/HasTopK/HasTDigest 此前恒 false**：
`INFO MODULES` 报出的是模块加载名（`bf`、`cb`、`RedisBloom` 等），而
`cf`/`cms`/`topk`/`tdigest` 只是**命令前缀**、不是模块名——旧实现拿前缀去查
模块名，四个判定在任何服务器上都恒 false。后果不止误报：`NewCuckooFilter`
以 `HasCuckoo()` 选实现路径，于是装了 RedisBloom 的服务器也被静默降级到
Lua 回退实现（丢掉模块的自动扩容等能力，键结构还不通用）。

v0.7.0 起改为**分层探测**：先由 `INFO MODULES` 确认 bf 在场（这是
`HasBloom()` 的唯一口径——bf 在场 ⇒ BF.* 可用，在 RedisBloom、valkey-bloom、
Redis 8 内建等形态下均成立）；bf 在场时再用
`COMMAND INFO CF.ADD / CMS.MERGE / TOPK.ADD / TDIGEST.ADD` 逐族真实确认，
结果缓存进四个独立字段。bf 不在场则四族直接为 false（省 4 个往返）。因此
下列形态的判定均正确：valkey-bloom（只有 BF 族，四族 false）、旧版
RedisBloom（缺 CF/CMS/TOPK/TDIGEST 中某几族，按实际逐族给值）、Redis 8 裸
二进制（无模块条目但有 BF.*，`HasBloom` true、四族 false）、以及探测期
服务瞬断（错误原样返回、**不**被当作"不支持"缓存，下次访问重试）。

**升级影响面（属纠正）**：只有"服务器本来有 CF.*、却因旧版误判一直用着
Lua 回退版"的用户会看到行为切换——自动改走 CF.* 路径，旧回退键读不到，
需重建过滤器。原本就没有 CF.* 的部署不受本次修复影响，继续走回退版（其
存量数据受上文哈希换源影响需重建）。若代码里有把 cf/cms/topk/tdigest 前缀
当模块名传给 `HasModule` 判可用性的用法，请改用对应的 `HasCuckoo()` /
`HasCMS()` / `HasTopK()` / `HasTDigest()` 判定函数。


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
| 布隆/布谷鸟过滤器 | FailOpen | Add/Exists 返回成功值（防穿透失效但放行业务）；观测方法 Card/Count 返回 (0, ErrRedisUnavailable 哨兵)，不适用放行/拒绝二值 |
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
