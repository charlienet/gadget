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

分布式锁（SET NX EX + Lua 原子释放/续期）：

```go
lock := rdb.NewLock("job:1", redis.WithTTL(10*time.Second))
if ok, err := lock.TryLock(ctx); err != nil || !ok {
    return err // 锁被他人持有
}
defer lock.Unlock(ctx) // 仅 token 匹配时删除，防误删他人锁
// Renew(ctx, ttl) 可作看门狗续期；WithToken 可指定 token 检测重入
```

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
