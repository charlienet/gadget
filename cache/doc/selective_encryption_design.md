# cache 按 key 选择性加解密 · 设计定稿

> 状态：设计定稿（经三轮裁决收敛：v1 框架 → v2 职责边界修订 → v3 解密失败语义修订），未实现。
> 范围：独立 module `github.com/charlienet/gadget/cache`。新增公开 API → 发 **minor** 版本。
> 前置事实基线：全路径编解码已统一走 `Serializer`（见 `serializer.go` 的 `unmarshalAny`）；`Cipher` 接口与 `WithCipher` 注入通道已存在（`cipher.go`、`options.go:224`）；存储值已带 9 字节版本头（`wrapVersion`，`cache.go` 中 `versionMarker=0xFB`）。

---

## 1. 背景与目标

现有 `WithCipher` 是**全局**透明加解密：注入后所有 key 在 L1/L2 都存密文。目标演进为：

1. **按 key 规则选择性加密**：只有命中规则（通配符，如 `*`、`:test*`）的 key 在**远程存储（L2）**存密文，其余保持明文。
2. **多条规则可共用同一个 Cipher 实例**（规则 → Cipher 多对一）。
3. **职责边界**：cache 只负责「路由」——匹配 key → 调对应 Cipher → 在结果头加"是否加密"标志 + 实例标识；密钥轮换、密钥序号、iv/nonce 等信封结构全部是 **Cipher 实现的内部协议**，cache 永不解析密文内部字节。
4. **读路径零 key 匹配**：取回数据靠存储字节自描述判别是否解密、路由到哪个 Cipher，不在读路径重跑规则引擎。
5. L1 本地内存缓存不加密（威胁模型：密钥与进程内存共存亡）。

### 非目标（明确不做）

- 密钥轮换 / 密钥版本化：归 Cipher 实现（见 §5 两层信封），cache 协议永久不涉密钥代次。
- KMS / 密钥注入框架：`Cipher` 由应用注入，密钥来源是应用的事。
- 规则变更后的在线重加密 / 后台迁移任务。
- L1 加密开关（若未来出现"运维可查 L1 但不可见密级数据"需求再议）。
- 规则语法的 `?`、正则、排除规则；写路径匹配结果缓存。
- key 名脱敏：本设计只加密 value，key 在 Redis 中保持明文（key 掩码/哈希是另一个 feature）。
- remote 侧 bulk 读路径（现状 `GetMulti` bulk 仅对 localStore 生效，`cache.go:309` 区段，不新增）。
- 解密失败限速/退避（防恶意刷坏值放大回源）：计数先行，必要时再议。
- `Metrics` 接口扩展、hard-fail 配置开关、跨实例自动密钥校验通信。

---

## 2. 裁决速览（定稿结论一览）

| # | 议题 | 裁决 |
|---|---|---|
| 1 | 读路径判定 | **数据自描述**（头 magic 判别，纳秒级），key 重匹配仅作存量 legacy 回退；热读路径规则引擎完全移出 |
| 2 | L1 是否加密 | **L1 恒明文，L2 按规则加密**；省掉热读每次 Decrypt 的 CPU（AES-GCM 约 0.5~2µs/次） |
| 3 | 格式 | `0xFB`=明文、`0xFC`=密文双 magic；版本头恒 9 字节不变；slotID 2 字节放 payload 前缀、不进头 |
| 4 | 实例标识分配 | **显式槽位 ID**（`WithCipherSlots` 注册 + 规则引用），否决"注入顺序自动编号"——slotID 是烙进 Redis 字节的跨实例路由契约，必须可配置、可比对、可 lint |
| 5 | 密文内部结构 | opaque：密钥序号/nonce/tag 归 Cipher 自定义，cache 从 slotID 之后一字节都不碰；`Cipher` 接口一字不改 |
| 6 | 与 `WithCipher` 共存 | 语义升级为"兜底规则 `*`"（最低优先级）。**写入格式以"是否配置 `WithEncryptRules`"为切换条件**：未配置（仅 WithCipher）→ 密文维持现有 `0xFB|ts|密文` 格式，读写与旧实例完全互容，存量用户灰度升级零破坏；配置后 → 兜底密文才写 `0xFC|ts|slotID=0|密文`，该集群受两阶段发布纪律约束（§10.1） |
| 7 | 规则语法 | 仅 `*` 通配 + 字面量，按 `*` 切段：首段前缀锚定、中段子串、尾段后缀锚定；按注入顺序首命中生效；规则顺序**不影响** slotID |
| 8 | 写路径匹配成本 | 预编译分段匹配、不加缓存：<50 规则 <1µs/次，比 Redis RTT 低 2~3 个数量级，且仅发生在写路径 |
| 9 | 解密失败语义 | **分本质混合**：永久失败（换钥/换绑/未知 slotID/legacy 全败/篡改）→ **自愈**（删条目 + miss 语义 + 计数告警）；瞬时失败（KMS 不可达等，Cipher 经可选接口声明）→ **fail-fast**（报错、条目保留）。自愈以可观测性契约为强制前置 |
| 10 | 元数据存哪 | 与 value 同址前缀（头+slotID 全在同一个 Redis string 内）：一次 GET 原子取回、TTL 共生死、MGET/bulk 兼容；否决独立 meta key（双 RTT + 孤儿状态 + 每 key ~50-90B 固定开销放大）与 Hash 双字段（MGET 报废、Store 接口重造） |
| 11 | 发布节奏 | 阶段 0（格式+L1 明文，不可逆项一次定案）→ 阶段 1（规则引擎+多 Cipher+失败协议），可同 release |

---

## 3. 存储格式（定稿）

```
明文（存量 + 新写未命中任何 Cipher）:
  0xFB | ts(8B, 毫秒) | plaintext-payload

密文（新写命中规则/兜底）:
  0xFC | ts(8B, 毫秒) | slotID(2B, BigEndian) | opaque-ciphertext

旧格式（无版本头存量数据）:
  原始 payload                        ← versionOf()==0，按明文处理

空值占位符（防穿透）:
  "*"                                 ← 纯 ASCII，永不加密、无版本头（seal 双侧短路）
```

### 3.1 设计要点与依据

- **头恒 9 字节**：`versionPrefixLen=9` 是 `wrapVersion`/`payloadOf`/`versionOf`/降级 flush 全链路的硬假设；slotID 放 payload 前缀，`payloadOf` 行为不变，"剥 slotID"只在判定 0xFC 后追加一步。slotID 进头会改头长、牵动全部剥头点，收益为零。
- **标志放 magic、不设 header flag 位**：现有判据已是 `data[0]==0xFB`，双 magic 是认知差最小的扩展；位运算拆 flag 复杂化"字节自描述"。
- **`0xFC` 与 `0xFB` 同为非合法 UTF-8 首字节**（现代 UTF-8 首字节 ≤0xF4），JSON/文本明文永不混淆——同一自描述赌注、同强度。
- **slotID 宽 2 字节**：Cipher 实例数量级小于规则数，1B 理论够用；维持 2B 是为避免"超 255 实例"的格式二次迁移（格式不可逆，宁宽勿悔）。启动期断言上限 `maxCipherSlots`（建议 256）。
- **ts 继续参与版本比较**：`syncBatch`/verify 只比 ts，**零改动**；密文的 ts 在加密范围之外，跨实例版本比对不解密。
- **value 是原始二进制字节，非 hex/base64**：Redis bulk string 二进制安全（RESP 带长度前缀），go-redis 对 `[]byte` 原样收发，`MGET/MSET/SCAN` 均兼容；hex 膨胀 2× 无任何收益。代价：redis-cli 肉眼读值有 `\xfb`/`\xfc` 转义前缀，调试可 `GETRANGE key 9 -1` 跳过 9 字节头。需要"Redis 内纯 JSON 可读"的数据属于"不该过本 cache 层"的场景，不提供跳过 wrapVersion 的开关（会破坏版本同步语义）。
- **三形态共存判别**：全部由首字节一次比较完成（`0xFB`/`0xFC`/其他=裸存量），读路径零额外访问。
- **0xFC 写入切换条件（灰度兼容硬约束）**：仅当集群**显式配置了 `WithEncryptRules`** 时才写 0xFC 密文；未配置规则（即使有 `WithCipher` 兜底）时，密文一律维持现有 `0xFB|ts|密文` 形态（与明文/裸存量一样经 legacy 候选序列解密，§7.2）。理由：旧版本实例不认识 0xFC、读到必报错——若 WithCipher 用户一升级就改写密文格式，其自身滚动升级期（新实例写、旧实例读）即被打破；而旧格式密文对新旧实例双向互容，纯兜底用户升级零暴露面。两阶段发布纪律（§10.1）自此只约束"开规则"的集群。

### 3.2 两层信封对照（cache 协议层 vs Cipher 信封层）

```
存储字节:
┌─────────────────────────────┬──────────────────────────────────────────┐
│ cache 协议层（本设计定义）    │ Cipher 信封层（实现自定义，cache 不解析）    │
│ [0xFC][ts 8B][slotID 2B]   │ [1B 密钥序号][12B nonce][GCM 密文+tag] …   │
└─────────────────────────────┴──────────────────────────────────────────┘
```

- cache 交给 `Decrypt` 的字节从首字节起就是 Cipher 自己的信封；Cipher 无需感知 magic/ts/slotID。
- README 已发布的参考实现 `RotatingCipher`（密文 `[1B keyID][12B nonce][GCM…]`，keyID 1~12 自管）**天然吻合本边界，无需改写**；仅需将其文档定位从"cache 轮换方案"改为"Cipher 侧信封示例"，并把 README 存储布局表（"L1/L2 统一存密文"旧表述）更新为本文 §4 的分层模型。
- 轮换时 cache 零改动：应用调 Cipher 的 `Rotate`，新旧代密文由 Cipher 自识别；cache 的 slotID、格式、代码全不动。

---

## 4. L1 恒明文 / L2 按规则加密

**裁决：采纳。** 理由：

1. 威胁模型成立：L1 加密防的是内存转储，而密钥同驻进程内存，攻破进程者通常也拿到密钥，L1 加密只剩混淆价值。
2. 热读路径每次 `Decrypt` 约 0.5~2µs，万级 QPS 进程内是可观测 CPU 占比；L1 明文后热读零加解密（写路径 Encrypt 仍一次）。
3. 复杂度可承受：改动点全部按"数据来源/目标"判定（不需要 key 匹配），已逐处盘清（§9 清单）。
4. 现状内部搬运（`syncBatch` 回写 L1、`getFromCacheData` 回填、pending flush 回写 L2）全程不解密原样搬运字节——**一旦分层，"字节形态"必须随数据跨层传递**，故各搬运点显式携带 sealed 标志（见 §9）。

**书面声明的代价**：L1 明文意味着进程内 value 对内存检查者可见。

**读写收口重排**（关键结构调整）：

- `seal` → `sealForKey(key, data) (payload []byte, sealed bool, err error)`：匹配规则 → 命中则 `slots[slot].Encrypt(data)` 并置 sealed（写 0xFC+slotID）；未命中走兜底（`WithCipher`，有则加密，无则明文）。**兜底密文的格式随是否配置规则切换（§3.1）**：未配置 `WithEncryptRules` → 写 `0xFB|ts|密文`（sealed=true 但保持旧字节形态，读取走 legacy 候选序列）；已配置 → 写 `0xFC|ts|slotID=0|密文`。
- 出口 `unseal` **删除**；**唯一解密点下沉到 `getFromCacheData`**（数据从 L2 进入进程处）：sealed → 取 slotID 路由 Cipher → `Decrypt` → 返回明文，回写 L1 的已是明文。
- `getFromCache`（singleflight 层）、`GetMulti` bulk 命中路径的出口 unseal 相应删除（L1 恒明文，读 L1 无需任何解密调用）。

---

## 5. 规则语法与匹配成本

- 操作符仅 `*`（任意字符序列，任意位置、可多个），其余为字面量。
- 按 `*` 切分为字面段（预编译为 `[][]byte`）：首段锚定前缀、中段 `bytes.Contains` 顺序查找、尾段锚定后缀。示例：`*` 全匹配；`:test*` 前缀匹配；`*:user*` 含子串。
- 规则按注入顺序匹配、**首个命中生效**。规则顺序变更只影响"key→slot 映射"（新写入路由与 legacy 候选序），**不影响 ID→Cipher 注册表**（显式 ID 的直接收益）。
- 成本结论：写路径（`putCache`、`Getfn` 回填、`SetMulti`）每 key 一次匹配，<50 规则 <1µs，被 Encrypt（1~3µs）、序列化（1~5µs）、Redis RTT（0.1~1ms）淹没；热读零匹配（自描述）。**不加匹配结果缓存**——引入规则变更失效语义而收益不可测；规则到千条级时以纯内部改动（如 sync.Map 缓存）向后兼容地优化。

---

## 6. API 设计（签名草案，定稿以本文件为准）

```go
// options.go

// CipherSlot 注册一个 Cipher 实例到显式槽位。
// ID 是跨实例稳定的路由键：同一 ID 在集群内各实例必须指向"同一密钥"的 Cipher 实现。
// ID=0 保留给 WithCipher 兜底，显式注册不得使用。
type CipherSlot struct {
    ID     uint16
    Cipher Cipher // 非 nil；同一实例可被多条规则引用（多对一共用）
}

// EncryptRule 描述一条选择性加密规则。
// Pattern：仅 '*' 通配 + 字面量（§5 语义）。Slot：引用已注册槽位。
// 规则按注入顺序匹配、首个命中生效。
type EncryptRule struct {
    Pattern string
    Slot    uint16
}

// WithCipherSlots 注册 Cipher 实例表。重复 ID、ID=0、nil Cipher、超上限 → New panic。
// 注册了但无规则引用：合法（两阶段发布友好——先注册后上规则）。
func WithCipherSlots(slots ...CipherSlot) Option

// WithEncryptRules 注入加密规则集。引用未注册 Slot、Pattern 非法 → New panic。
// 空规则集 = 退化为兜底行为。
func WithEncryptRules(rules ...EncryptRule) Option

// WithCipher 语义升级（代码不动、文档改写）：等价兜底规则 '*'（最低优先级），
// WithCipher 语义升级（代码不动、文档改写）：等价兜底规则 '*'（最低优先级）。
// 规则未命中时：有兜底 → Encrypt；无兜底 → 明文。
// 写入格式切换（§3.1）：未配置 WithEncryptRules 时，兜底密文维持现有
// 0xFB|ts|密文 字节形态，与旧版本实例双向互容——存量全量加密用户升级
// （含滚动灰度）零行为变化、零格式暴露面。配置 WithEncryptRules 后，兜底
// 密文改写 0xFC|ts|slotID=0|密文，集群进入两阶段发布纪律约束范围。
func WithCipher(c Cipher) Option
```

用法示例：

```go
cache.New(
    cache.WithCipherSlots(
        cache.CipherSlot{ID: 1, Cipher: userCipher},
        cache.CipherSlot{ID: 2, Cipher: orderCipher},
    ),
    cache.WithEncryptRules(
        cache.EncryptRule{Pattern: ":user*",    Slot: 1},
        cache.EncryptRule{Pattern: ":profile*", Slot: 1}, // 与上条共用 slot 1
        cache.EncryptRule{Pattern: ":order:*",  Slot: 2},
    ),
)
```

错误策略：`New` 无 error 返回（cache.go:183），配置非法一律 **启动期 panic**——静默忽略会制造"部分 key 明文落 L2"的假安全。与 `WithCipher` 现"忽略 nil"不一致处，以文档显式说明（新 API 走 fail-fast，旧行为不动）。

### 6.1 瞬时失败分类（可选能力探测，不并入 Cipher）

```go
// Cipher 可选实现：声明 Decrypt 失败的瞬时类（KMS 不可达、密钥服务超时等）。
// cache 据此保留条目并返回 error（fail-fast），而非误判条目无效。
// 未实现本接口的 Cipher：默认所有失败按永久处理（自愈）。
type TransientDecryptError interface {
    IsTransientDecryptError(err error) bool
}
```

文档强约束：凡 `Decrypt` 依赖外部可达性（KMS/网络）的实现**必须**声明瞬时分类，否则瞬时故障会被自愈误删条目（§8 反例推演）。内存密钥型实现（如 README RotatingCipher）无需实现。

### 6.2 一致性工具（可选）

```go
// 可选接口：跨实例比对指纹（不得泄露密钥材料）；未实现则摘要降级为类型名比对。
type CipherDescriptor interface {
    ConfigFingerprint() string
}

// 公开摘要：按 ID 排序输出；两实例"ID 集合 + 每 ID 的 Type/Fingerprint"全等 ⇒ 注册表一致。
// 供应用接配置中心/启动健康检查，cache 自身不做任何分布式通信。
func (c *cache) EncryptionConfigSummary() EncryptionConfigSummary
```

---

## 7. 读路径与 legacy 回退

### 7.1 正常读（新格式，零 key 匹配）

```
store 取出 → 判 data[0]：
  0xFC → 剥 9B 头 → 取 slotID(2B) → slots[slotID].Decrypt(opaque) → 明文
  0xFB → 剥 9B 头 → 明文
  其他 → 裸存量，按明文
```

`getFromStore` 返回值扩为携带 `sealed bool`（剥头前判 `data[0]`，剥头动作现状会丢失 magic 信息）——改动仅 `getFromCacheData` 两个调用点 + health probe 忽略。

### 7.2 legacy 回退（存量 `0xFB|ts|X`，X 为旧全局密文时与明文不可区分）

存量第三类：现版本 `WithCipher` 写入的 `0xFB|ts|密文`（payload 无标志）。处理——**候选序列**：

1. 候选集（去重保序）：该 key 今日命中规则对应的 Cipher（多规则命中则按序收集 distinct slot）+ 兜底 `WithCipher`（若存在且未重复）。
2. 候选集非空才尝试：依次 `Decrypt`，首个成功即还原。
3. 全败 → 进入解密失败处理协议（§8）永久类：删条目 + miss 语义 + 计数。
4. 成本：每次失败尝试 = 一次 AEAD open 认证失败（µs 级），仅存量窗口发生，随 TTL/首读收敛消亡。
5. 0xFC 数据永不进 legacy 路径（slotID 已定向路由）。

候选序列同时封掉"旧全局密文的 key 今日已落入明文域"的静默脏值洞：不按"今日是否命中"决定是否尝试，兜底实例恒在候选内；失败也不报错而是自愈重写。

### 7.3 向后兼容矩阵（新实例读存量）

| 存量数据 | 判定 | 行为 |
|---|---|---|
| 裸数据（无头） | 非 0xFB/0xFC | 明文直读 |
| `0xFB\|ts\|明文` | 0xFB + 候选解密失败或无候选 | 明文直读（无候选）；候选全败 → 自愈删除回源（此路径误删概率与收敛代价已在协议内，计数可见） |
| `0xFB\|ts\|旧全局密文` | 0xFB + 候选命中 | 兜底/今日规则 Cipher 解回 |

---

## 8. 解密失败处理协议（定稿核心裁决之一）

**按失败本质分类，不按运维场景**（cache 无法知晓意图）：

| 失败本质 | 判定标准 | 行为 |
|---|---|---|
| **永久失败**：篡改/损坏、slot 换绑/换密钥、Retire 过早、未知 slotID、legacy 候选全败 | `err != nil` 且未被声明瞬时 | **自愈**：删条目 + miss 语义 + `DecryptFailPermanent` 计数 + Warn 日志 |
| **瞬时失败**：KMS 不可达等环境故障 | Cipher 实现 `TransientDecryptError` 且判定为瞬时 | **fail-fast**：返回 error、**条目保留** + `DecryptFailTransient` 计数 + Warn 日志 |

自愈协议（判定点：`getFromCacheData` 唯一解密处）：

```
永久类：
  stats.IncrDecryptFailPermanent()
  logger.WarnContext(ctx, "undecryptable cache entry removed, will refill from source", key, slotID, err)
  removeFromStorage(localStore, key)   // 复用现成：降级期自动入 pendingDeletes
  removeFromStorage(remoteStore, key)
  resetVerifyCount(key)
  返回 miss → Getfn: fn 回源回填；Get: ErrEntityNotExist；GetMulti: 该 key 不出现在 result map
```

**删除动作裁决**：同步双删本实例 L1+L2、**不广播**（不调 `noticeRemoved`）。理由：

1. 解密失败是本实例局部判断，其他实例配置可能正常，广播会误伤健康实例。
2. 只发生在异常条目上，热读路径零额外 RTT。
3. 不采用"只删 L1 靠回填覆盖"：纯 Get 调用方可能从不回填，坏值永驻 remote。

**为什么不是无条件统一 miss**：KMS 瞬时故障期间，每个读把有效密文判无效 → 删空缓存 → 回源风暴，且回填依赖 Encrypt（同样失败）→ 穿透击穿后端。失败分类是自愈的安全前置，不是可选增强。

**自愈 ≠ 掩盖**：删除解不开的条目 + 回源真相 + 按当前配置重写，本身就是纠正；"暴露漂移"职能完整移交可观测性契约（§8.1）。分场景推演（轮换 Retire 过早 / 换绑换密钥 / 实例间漂移 / 篡改投毒）下自愈均优于或等于 fail-fast，详见裁决速览 #9 与各节论证。

**不加热 fail 开关**（YAGNI）：自愈仅覆盖"永久损坏"类，语义与 miss 同构；双行为矩阵长期成本不值；未来确有需求属向后兼容的纯行为开关，不预留。

**验收仲裁**：正常运行时 `DecryptFailPermanent` 应恒为 0——零即健康，任何非零读数即运维事件（换钥/漂移/投毒）。**若实现未做到失败分类 + 双计数，本自愈裁决作废，回退纯 fail-fast。**

### 8.1 可观测性契约（自愈的强制前置）

```go
// stats.go（原子计数 + Snapshot 深拷贝模型，加字段零成本）
type Stats struct {
    // …既有字段…
    DecryptFailPermanent uint64 // 永久解码失败 → 条目已删回源（自愈）
    DecryptFailTransient uint64 // 瞬时解码失败 → 条目保留、error 返回（fail-fast）
}
```

- **不动 `Metrics` 接口**（仅 `CacheEviction/SetDegraded` 两方法，扩展是破坏性变更）；Prometheus 等由使用方从 `Stats()` 快照自行导出。
- 告警建议（入 README 运维节）：`rate(DecryptFailPermanent) > 0` ⇒ 配置漂移/换钥/投毒嫌疑；`rate(DecryptFailTransient) > 0` ⇒ 环境故障。

---

## 9. 改动面清单（行号基于当前工作树，实现时以就近代码为准）

| 位置 | 改动 |
|---|---|
| `cache.go:44-45` 常量区 | 增 `sealedMarker=0xFC`、`slotIDLen=2`、`maxCipherSlots=256` |
| `cache.go:88-96` struct | `cipher` 语义改 `defaultCipher`（兜底，slot 0）；增 `slots []Cipher`、`rules []compiledRule` |
| `cache.go:143-146` `pendingWrite` | 增 `sealed bool` |
| `cache.go:183-206` `New` | 注册表 + 规则编译 + 校验 panic（清单见 §6） |
| `cache.go:1091-1100` `seal` | 改 `sealForKey(key, data) (payload, sealed, err)`：规则匹配 → Encrypt + slotID 前缀 |
| `cache.go:1104-1113` `unseal` | 删除（解密点下沉） |
| `cache.go:1115-1132` `putCache` | 按层分叉：L1 写序列化明文；L2 写 sealForKey 结果 |
| `cache.go:1207-1246` `putInStore` | 签名增 sealed；wrap 按 sealed 选 magic；降级入 pending 记录 sealed |
| `cache.go:1252-1328` `flushPending` | 按 `pw.sealed` 选 magic wrap |
| `cache.go:1134-1168` `getFromStore` | 返回值携带 sealed（剥头前判 `data[0]`） |
| `cache.go:998-1056` `getFromCacheData` | **唯一解密点**：自描述解密 + legacy 候选回退 + §8 失败协议（删除/miss/计数/日志）；回写 L1 前已明文 |
| `cache.go:1058-1086` `getFromCache` | 出口 unseal 删除 |
| `cache.go:309-351` `GetMulti` bulk | unseal 删除（L1 恒明文）；失败 key 不出现在 result map |
| `cache.go:374-442` `SetMulti` bulk/fallback | 按目标 store 分叉（local 明文 / remote sealed）；bulk `wrapVersion`（~399）按 sealed 选 magic |
| `cache.go:857-912` `syncBatch` | `versionOf` 兼容双 magic；`rv>lv` 回写 L1 前将密文解码为明文 |
| `cache.go:1398-1422` 包装函数族 | magic 集扩 {0xFB,0xFC}；增 `isSealed`、`slotIDOf`、`wrapPayload(payload, sealed)` |
| `options.go:219-230` | `WithCipher` 文档升级；新增 `WithCipherSlots`/`WithEncryptRules`/`EncryptRule`/`CipherSlot` |
| `cipher.go` | 增可选接口 `TransientDecryptError`（`Cipher` 本体不动） |
| `stats.go` | 增两个 `atomic.Uint64` 字段 + Snapshot/Clear 同步处理 |
| 新增 `cache/encrypt.go` | 规则解析/编译/匹配、slot 注册表、`sealForKey`、`decodePayload(key,data)`、legacy 候选、失败协议 |
| 新增（encrypt.go） | `CipherDescriptor` + `EncryptionConfigSummary` |
| `README.md`（280-284 布局表、286-419 RotatingCipher 节） | 更新为分层存储模型 + 两层信封对照 + §8 失败协议与告警表；RotatingCipher 重定位为"Cipher 侧信封示例" |
| 测试 | `cipher_test.go`/`cache_internal_test.go` 旧密文格式假设适配 v2 格式；新增见 §11 |

明确不改（缩小审查面）：`Serializer` 全路径与 `unmarshalAny`、`respond`、`removeFromStorage` 本体（直接复用）、`DeletePattern`、listener/PubSub 本体、`healthLoop` probe、verify 版本比较（只比 ts）、memory_store 本体、`Metrics` 接口。

---

## 10. 发布与运维纪律

1. **两阶段发布（保留，约束范围收窄）**：**仅适用于启用 `WithEncryptRules` 的集群**——先全量升级所有实例到本版本，再启用规则。职能是缩小旧实例误读窗口——**旧版本库没有 0xFC 判别与自愈路径，读到新格式密文只能报错**，混布期新格式数据不得由旧实例消费。新实例侧全部异常由失败协议接管（永久→自愈，瞬时→短暂 error）。未配置规则的纯 `WithCipher` 用户不写 0xFC（§3.1 切换条件），滚动升级双向互容，不受本纪律约束。
2. **注册表一致（唯一跨实例硬约束）**：同一 slotID 在各实例必须指向同一密钥的 Cipher 实现（含 ID=0 兜底）。不一致的后果不再是业务报错，而是错配实例持续 miss→删→按本地配置重写，表现为命中率损失与 `DecryptFailPermanent` 速率飙升（分钟级告警捕获）。`EncryptionConfigSummary` 供配置中心启动期比对。
3. **轮换归 Cipher**：cache 侧零动作。`Retire` 过早的后果从 v2 的"存量持续报错"改述为"存量被自愈删除并回源重写（隐式迁移）"，实现侧"须待旧密文 TTL 过期方可摘钥"的警告保留。
4. **换绑/换密钥（slot 重注册）**：发布新注册表后，旧密文由首读自愈收敛，无需人工 `DeletePattern`；大面积回源由 singleflight + 空值缓存 + TTL 抖动兜底，`DecryptFailPermanent` 速率告警盯梢。
5. 恶意刷坏值 key 集可放大删除/回源：本版不设限速，已列非目标，文档声明。
6. 版本语义：新增公开 API → 独立 module minor；tag 打在实现评审通过后的最新提交（远程发布门禁）。

---

## 11. 实现验收对标清单（评审对照，全绿才可发布）

**格式与路由**

1. L1 恒明文：mem_store 内任何条目 payload 不含 0xFC 头/密文特征（直接断言字节）。
2. 读路径零 key 匹配：`GetMulti` bulk 命中与 `getFromCache` 路径无规则匹配调用（计数断言）。
3. 兜底格式切换：仅 `WithCipher`（无规则）→ 新密文保持 `0xFB|ts|密文` 旧形态且新旧实例读写互容（滚动升级仿真断言）；配置规则后 → 兜底密文头 slotID=0x0000、magic=0xFC。
4. 两层信封消毒：拦截 Cipher 的 Decrypt 入参，断言首字节即信封自有结构、不含 slotID/0xFC/ts；Encrypt 返回值不含协议头。
5. 多对一共用：三规则引用同一 Slot → slotID 相同、单实例解回。
6. 占位符 `*` 永不带 0xFC（seal 短路保持）。

**兼容与搬运**

7. legacy 三态：存量旧密文 + 今日命中 → 解回；不命中但有兜底 → 兜底解回；全败 → 自愈删除 + miss + 计数（**v3 改：不再断言报错**）。
8. 存量明文命中今日规则 → 自愈重写为密文的收敛断言。
9. syncBatch 跨格式：L1 明文(0xFB) + L2 密文(0xFC) 同 key，`rv>lv` 回写后 L1 为明文且 ts 与 remote 一致。
10. 降级 flush：降级期写命中规则 key → pending 记 sealed → 恢复后 remote 得到完整 `0xFC|ts|slotID|cipher`。
11. 轮换端到端（cache 零改动见证）：仅 Rotate Cipher 密钥，cache 配置/代码不动，新旧两代密文均可读，头字节恒 `0xFC|ts|slotID`。

**失败协议**

12. 永久失败自愈：slot 挂错密钥 → `Get` 返回 `ErrEntityNotExist`（非 error/非 panic/非密文泄漏）；`Getfn` 回源且回填后正常可读；`DecryptFailPermanent==1`；坏条目已从 store 消失。
13. 瞬时失败保留：`IsTransientDecryptError==true` → error 透传、条目仍在、`DecryptFailTransient` 计数、Permanent 不增；未声明分类的 Cipher 失败按永久（自愈）处理。
14. 删除语义：失败删除不触发 `noticeRemoved` 广播（listener 零 Publish 断言）；降级期删除入 pendingDeletes、恢复后补删。
15. GetMulti：解密失败 key 不出现在 result map。
16. 可观测：`Stats().Snapshot()` 两新字段一致；`Clear()` 归零。

**配置校验**

17. panic 全覆盖：Slot 重复 / 显式占用 ID=0 / nil Cipher / 引用未注册 Slot / 非法 pattern / 超 `maxCipherSlots`；"注册无引用"不 panic。
18. 一致性摘要：注册表顺序不同但内容同 → 摘要相等；同 ID 不同密钥指纹 → 摘要不等；未实现 `CipherDescriptor` → 降级类型名比对可用。
19. 回归护栏：既有全部测试（含统一编解码回归集 `TestGetMultiUsesSerializerConsistently` 等）不回归；无规则 + 无 Cipher 的默认路径字节级行为与现版本一致（仍 `0xFB|ts|JSON`）。

---

## 12. 分阶段落地

- **阶段 0（格式定案，不可逆项，先行）**：双 magic **读取兼容** + 2B slotID 前缀 + `getFromStore` sealed 传递 + 解密点下沉 + L1 明文全链路 + `pendingWrite.sealed` + `syncBatch` 兼容。**本阶段不写 0xFC**（无规则 API，兜底密文维持 `0xFB|ts|密文` 旧形态），`WithCipher` 存量用户升级（含滚动灰度）零行为变化、零格式暴露面。验收：对标清单 1/3(前半)/4/7/9/10/19。
- **阶段 1（规则与多 Cipher）**：`WithCipherSlots` + `WithEncryptRules` + 0xFC 写入切换（配置规则后生效）+ legacy 候选序列 + 启动期校验 + 失败协议（分类/自愈/fail-fast/双计数）+ `EncryptionConfigSummary` + README 更新。可与阶段 0 同 release。
- 实现完成后按 §11 全清单 + 对照本文逐条做实现符合性评审，评审通过方可 commit/tag/push（远程发布门禁）。

---

## 13. 决策沿革（三轮收敛记录，供审计）

| 轮次 | 触发 | 实质变化 |
|---|---|---|
| v1 | 初始需求（按 key 选择性加密、读路径是否二次匹配、L1 是否加密） | 定下：自描述读路径、L1 明文、0xFC 双 magic、2B keyID、WithCipher=兜底、通配语法、两阶段发布、fail-fast 报错 |
| v2 | 用户边界修订（轮换归 Cipher；多规则共用一实例；cache 只加"是否加密"头与实例标识；密文内部 opaque） | keyID→**slotID（实例路由键）**语义改造；分配改**显式注册**（否决自动编号）；删除 cache 侧轮换演进勾画；legacy 回退升级候选序列；新增一致性摘要工具；RotatingCipher 确认无需改写 |
| v3 | 用户挑战 fail-fast（槽位变更后解不开→作废删除？） | 失败语义改**永久自愈 / 瞬时 fail-fast** 混合（分类为安全前置，否决无条件 miss）；可选接口 `TransientDecryptError`；同步双删不广播；`Stats` 双计数为可观测契约；"报错即信号"职能移交速率告警；两阶段发布保留但职能降级 |
| v3.1 | 用户复核"是否破坏对外 API"时自查发现格式矛盾（阶段 0 声称存量零变化与兜底密文改写 0xFC 冲突，滚动升级期旧实例读新密文必报错） | **0xFC 写入切换条件**（§3.1）：仅配置 `WithEncryptRules` 后才写 0xFC；纯 `WithCipher` 密文保持 `0xFB|ts|密文` 旧形态双向互容；两阶段发布纪律约束范围收窄至开规则集群；§10/§11/§12 同步修订。导出 API 面经 `go doc -all` diff 实证零破坏（已实现部分） |

补充问答定案（未改变裁决、已并入正文）：value 为原始二进制字节非 hex/base64（§3.1）；元数据与 value 同址前缀的备选方案否决理由（裁决速览 #10）；magic 标签与 ts 在格式演进中全部保留、判据扩为集合（§3.1）。
