package cache

import (
	"time"

	"github.com/charlienet/gadget/logger"
)

// Options represents the options for the cache.
//
// TTL 有效域：1 ~ 9e9 秒（内部以 int 秒数累加，超出 int 范围可能溢出）；
// <= 0 表示永不过期（与 Put 的 expireSecond 语义一致）。
type Options struct {
	localStore          Store
	remoteStore         Store
	listener            Listener
	serializer          Serializer
	metrics             Metrics
	Logger              logger.Logger
	TTL                 int
	Name                string
	cleanupInterval     time.Duration
	maxItems            int
	maxBytes            int64
	degradeThreshold    int
	degradeRecovery     time.Duration
	ttlJitter           time.Duration
	ttlJitterSet        bool // 是否显式调用过 WithTTLJitter（区分"未设置"与"显式 0 关闭"）
	slidingWindow       time.Duration
	verifyEvery         int
	versionSyncInterval time.Duration
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

func WithTTL(ttl int) Option {
	return func(o *Options) {
		o.TTL = ttl
	}
}

func WithLogger(l logger.Logger) Option {
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

// WithVerifyEvery enables probabilistic local cache verification. After N local
// cache hits for a key, the next Get skips local and checks remote. If remote
// no longer has the data, the stale local entry is cleared immediately.
// 0 (default) disables verification.
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
