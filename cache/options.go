package cache

import (
	"log/slog"
	"time"
)

// Options represents the options for the cache.
//
// TTL 为全局默认过期秒数：未调用 WithTTL 时默认 60s（defaultExpiresSeconds）；
// 显式 WithTTL 后生效，有效域 1 ~ 9e9 秒（内部以 int 秒数累加，超出 int 范围可能
// 溢出），<= 0 表示永不过期（与 Put 的 expireSecond 语义一致）。
type Options struct {
	localStore       Store
	remoteStore      Store
	listener         Listener
	serializer       Serializer
	metrics          Metrics
	Logger           *slog.Logger
	TTL              int
	ttlSet           bool // 是否显式调用过 WithTTL（区分"未设置→默认 60s"与"显式 <=0→永不过期"）
	Name             string
	cleanupInterval  time.Duration
	maxItems         int
	maxBytes         int64
	degradeThreshold int
	degradeRecovery  time.Duration
	ttlJitter        time.Duration
	ttlJitterSet     bool // 是否显式调用过 WithTTLJitter（区分"未设置"与"显式 0 关闭"）
	slidingWindow    time.Duration
	// hotKeyThreshold 是 L1 容量驱逐的热 key 豁免阈值：一个清理周期内命中 >= 该值
	// 的条目在驱逐时被优先跳过。<= 0 关闭（默认，退化为纯 LRU）。见 WithHotKeyThreshold。
	hotKeyThreshold     int
	verifyEvery         int
	versionSyncInterval time.Duration
	// delayedSecondDelete 是「延时二次删除」的延迟时长：Invalidate 完成首次双删后，
	// 再经该延迟对该 key 无条件补删一次，兜底清除延时窗口内落入缓存的旧值回填（含
	// "首删前读库、首删后才写入"的跨实例竞态脏值）。0（默认）关闭。须大于业务最坏「回源
	// 读库 + 回填缓存」耗时。见 WithDelayedSecondDelete。
	delayedSecondDelete time.Duration
	// storeSet 是否显式设置过任一 store（WithStore/WithMemStore），
	// 用于区分"零参 New 默认注入 mem_store"与"显式只配远程（只远程模式）"。
	storeSet bool
	// Cipher 提供透明加解密：缓存（L1/L2）中存储加密结果，调用方明文进出，
	// 无感知。nil 表示不加密（默认）。
	Cipher Cipher
}

func (o Options) init() {
	o.initActual(o.localStore)
	o.initActual(o.remoteStore)
	o.initActual(o.listener)
}

func (o Options) initActual(v any) {
	if v == nil {
		return
	}

	if i, ok := v.(interface{ Initialize(Options) }); ok {
		i.Initialize(o)
	}
}

func (o *Options) WithStore(s Store) {
	o.storeSet = true // 显式设置过任一 store（含只远程模式）
	if !s.IsRemote() {
		o.localStore = s
	} else {
		o.remoteStore = s
	}
}

func (o *Options) WithListener(l Listener) {
	o.listener = l
}

// Option manipulates the Options passed.
type Option func(o *Options)

func WithName(name string) Option {
	return func(o *Options) {
		o.Name = name
	}
}

func WithMemStore() Option {
	return func(o *Options) {
		o.WithStore(newMemStore())
	}
}

func WithStore(s Store) Option {
	return func(o *Options) {
		o.WithStore(s)
	}
}

func WithListener(lis Listener) Option {
	return func(o *Options) {
		o.listener = lis
	}
}

func WithSerializer(s Serializer) Option {
	return func(o *Options) {
		o.serializer = s
	}
}

// WithTTL 设置全局默认过期秒数（回填/回写路径使用）。不调用则默认 60s；
// 传 <= 0 表示永不过期。
func WithTTL(ttl int) Option {
	return func(o *Options) {
		o.TTL = ttl
		o.ttlSet = true
	}
}

func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

func WithCleanupInterval(d time.Duration) Option {
	return func(o *Options) {
		o.cleanupInterval = d
	}
}

func WithMetrics(m Metrics) Option {
	return func(o *Options) {
		o.metrics = m
	}
}

func WithMaxItems(n int) Option {
	return func(o *Options) {
		o.maxItems = n
	}
}

func WithMaxBytes(n int64) Option {
	return func(o *Options) {
		o.maxBytes = n
	}
}

func WithDegradeThreshold(n int) Option {
	return func(o *Options) {
		o.degradeThreshold = n
	}
}

// WithDegradeRecoveryInterval 设置降级恢复探测间隔。
// 非法值（<= 0）不会被应用（保持默认恢复间隔）；healthLoop 也仅在
// degradeRecovery > 0 时启动，避免 time.NewTicker 以非正 duration panic。
func WithDegradeRecoveryInterval(d time.Duration) Option {
	return func(o *Options) {
		if d > 0 {
			o.degradeRecovery = d
		}
	}
}

// WithTTLJitter adds a random jitter (0 ~ d) to every Put TTL to prevent
// multiple keys from expiring simultaneously (cache avalanche prevention).
// WithTTLJitter 设置 L1 内存层 TTL 随机抖动范围（防缓存雪崩）。
// 默认开启（0~30s 随机叠加，见 defaultTTLJitter）；传 0 显式关闭抖动；
// 传 d > 0 自定义抖动范围（TTL 叠加 0~d 随机值）。
func WithTTLJitter(d time.Duration) Option {
	return func(o *Options) {
		o.ttlJitter = d
		o.ttlJitterSet = true
	}
}

// WithSlidingWindow enables sliding expiration: a key's TTL is extended
// by its original TTL whenever it is accessed and the remaining TTL is
// less than the specified window. This prevents hot keys from expiring too early.
func WithSlidingWindow(d time.Duration) Option {
	return func(o *Options) {
		o.slidingWindow = d
	}
}

// WithHotKeyThreshold 开启 L1 内存层容量驱逐的热 key 豁免：在一个清理周期
// （WithCleanupInterval，默认 1 分钟）内命中次数 >= n 的条目被视为"热 key"，
// 容量驱逐时优先跳过、转而驱逐更冷的尾部条目，避免高频访问项被偶发批量写入挤出。
//
// 语义与边界：
//   - n <= 0（默认）关闭豁免，退化为纯 LRU。
//   - 豁免额度有上限：单轮驱逐最多跳过 budget = max(1, len(items)/4) 个热条目，
//     超过后一律驱逐（降级），保证 len <= maxItems 不变量恒成立；即便全部条目都
//     "够热"也不会撑破容量。
//   - 热度计数（hits）在每个清理周期结束时清零，故"热度窗口"等于 cleanupInterval；
//     窗口外的历史命中不计。
//   - 豁免只免"容量驱逐"，绝不免 TTL：惰性过期、后台过期清理、Delete、监听器失效、
//     版本同步发现远程已删而清本地，均无视热度照常生效。
//
// 仅对内置 mem_store（L1）生效。
func WithHotKeyThreshold(n int) Option {
	return func(o *Options) {
		o.hotKeyThreshold = n
	}
}

// WithVerifyEvery enables probabilistic local cache verification：第 N 次本地命中
// 当次即触发对 remote 的校验（内部实现为 count >= N，而非"命中 N 次之后的下一次
// Get"）。若 remote 已无该数据，则立即清除过期的本地条目。0 (default) disables
// verification.
func WithVerifyEvery(n int) Option {
	return func(o *Options) {
		o.verifyEvery = n
	}
}

// WithCipher 注入透明加解密器。
// 注入后：缓存（L1/L2）中存储的是 Encrypt 的结果（密文），Get/Getfn/GetMulti
// 返回前经 Decrypt 还原——调用方 Put/Get 无感知（明文进出）；空值占位符
// （notExistPlaceholder）不加密、明文存储。nil 参数被忽略（保留默认不加密）。
// 实现须并发安全。
func WithCipher(c Cipher) Option {
	return func(o *Options) {
		if c != nil {
			o.Cipher = c
		}
	}
}

// WithVersionSyncInterval enables a background goroutine that periodically
// checks local cache entries against the remote store and refreshes or
// evicts stale data. This provides pull-based cache coherence as a
// supplement to push-based PubSub invalidation.
//
// The goroutine iterates through local keys in batches (100 per cycle),
// comparing each key's value against the remote store. If remote has
// different data → update local. If remote no longer has the key → evict local.
// Skips the check when the cache is in degraded mode.
// Set to 0 to disable (default).
func WithVersionSyncInterval(d time.Duration) Option {
	return func(o *Options) {
		o.versionSyncInterval = d
	}
}

// WithDelayedSecondDelete 开启「延时二次删除」（delayed second delete）：Invalidate
// 完成首次双删后，再延迟 delay 对该 key 执行一次后台无条件补删，兜底清除「失效与并发
// 回源读库竞态」下、删除之后才回填进缓存的旧值（含首删前读库、首删后才写入的跨实例脏值）。
//
// 语义与代价：
//   - 采用经典无条件二次删：不判版本，直接对 L1/L2 各补删一次。因竞态回填脏值的写入版本
//     （毫秒时间戳）可能晚于失效时刻，按版本判定无法可靠识别，故无条件删除。
//   - 代价：窗口内的合法新写入也会被一并清除，由下次读取回源自愈（fail-safe）。
//   - delay 必须大于业务最坏「回源读库 + 回填缓存」耗时，否则回填可能晚于二次删发生而漏清。
//   - best-effort、非强一致：降级期跳过远程补删（由 TTL 与既有补偿重试兜底），且不写入
//     pendingDeletes，避免恢复后无条件误删新值。
//   - 量级提示：开启后每次 Invalidate 对每个受影响 key 正常产生 2 次 Publish
//     （首删 + 无条件二次删恒广播），高频写场景注意 listener 流量约 ×2。
//   - 0（默认）关闭此特性，零常态开销。
func WithDelayedSecondDelete(d time.Duration) Option {
	return func(o *Options) {
		if d > 0 {
			o.delayedSecondDelete = d
		}
	}
}
