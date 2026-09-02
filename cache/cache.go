// Package cache 提供两级（L1 内存 + L2 远程）缓存，带降级补偿、singleflight
// 去重与透明加解密能力。
//
// 日志约定：
//   - 未显式注入时，默认日志器由 slog.Default() 派生，并自动附加 module=cache
//     结构化属性，便于在混合输出中过滤本模块日志。
//   - 通过 WithLogger 注入的 logger 由调用方全权自理，本模块不会额外附加
//     module 属性；如需一致标注请在传入前自行 logger.With("module", "cache")。
//   - 请求路径上的日志使用 slog 的 XxxContext(ctx, ...) 方法（Warn/Error/Debug
//     Context 变体），配合 gadget logger 内置的 TraceHandler，可从 ctx 自动
//     注入 trace_id/req_id；后台协程（健康探测、版本同步、pending 补偿 flush
//     等自建 context.Background() 的路径）保持非 Context 变体。
//
// 推荐使用路径：
//   - 读路径用 Getfn：未命中时经 LoadFn 回源并自动回填（singleflight 防击穿）。
//   - 涉及源数据的更新或删除，一律经 Invalidate 包装：回调内写/删数据源，
//     成功后由库自动失效缓存（双删、补偿重试、集群广播），避免业务侧遗漏
//     清理造成缓存不一致；切勿在回调外手工"写源 + Delete"。
//   - Put/Delete 仅用于不涉及源数据的纯缓存操作（如临时计算结果）。
package cache

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	versionMarker              = 0xFB // magic byte prefix to distinguish versioned data
	versionPrefixLen           = 9    // 1 magic byte + 8-byte unix millisecond timestamp
	defaultVersionSyncBatch    = 100
	defaultVersionSyncInterval = 30 * time.Second
	// maxPendingWrites 是降级期间 pending 缓冲的最大条目数，防止长降级 × 高写速率下内存无界增长。
	maxPendingWrites = 1024
	// flushTimeout 是单次 flush 中逐条网络操作的总超时。
	flushTimeout = 3 * time.Second
	// flushRetryInterval 是常规（非降级恢复转换点）flush 重试的最小间隔，防止失败后网络风暴。
	flushRetryInterval = 5 * time.Second
	// defaultTTLJitter 是默认的 L1 内存层 TTL 随机抖动范围（0~30s），默认开启防缓存雪崩；
	// WithTTLJitter(0) 可显式关闭。
	defaultTTLJitter = 30 * time.Second
)

const (
	defaultCacheName           = "cache"
	defaultNotExistPlaceholder = "*"
	defaultExpiresSeconds      = 60
)

var (
	ErrEntityNotExist = errors.New("entity does not exist")
	// ErrPendingWritesFull 在降级期间 pending 缓冲达到上限时由 Put 路径返回。
	ErrPendingWritesFull = errors.New("cache: pending writes full")
	// ErrRemoteUnavailable 表示远程存储（L2）不可用或操作失败（网络错误、超时、
	// 远程拒绝等）。使用端可用 errors.Is 区分"远程故障"与"键不存在"等业务语义，
	// 决定是否降级处理（如返回兜底数据）。原错误链保留（errors.Is(err, 原始错误)
	// 仍可用）；context.Canceled（用户主动取消）不包装。
	ErrRemoteUnavailable = errors.New("cache: remote store unavailable")
)

type Cache interface {
	Get(ctx context.Context, key string, v any) error
	Getfn(ctx context.Context, key string, v any, fn LoadFn, expireSeconds int) error
	Put(ctx context.Context, key string, v any, expireSecond int) error
	Delete(ctx context.Context, keys ...string)
	Close()
}

type cache struct {
	localStore          Store        // 堆缓存
	remoteStore         Store        // 远程缓存
	listener            Listener     // 异步消息通知
	serializer          Serializer   // 序列化
	notExistPlaceholder []byte       // 缓存击穿空对象
	logger              *slog.Logger // 日志
	opt                 Options
	cipher              Cipher // 透明加解密器（nil 表示不加密）
	sg                  singleflight.Group
	stats               Stats
	metrics             Metrics
	ttl                 int
	stopChan            chan struct{}

	// Degraded mode
	degraded         atomic.Bool
	degradeCount     atomic.Int64
	degradeThreshold int
	degradeRecovery  time.Duration
	degradeStopRecov chan struct{}

	// Pending ops buffered while degraded, flushed when remote recovers.
	// Prevents the version sync / verify path from evicting local-only data
	// written during the degraded window, and prevents deleted data from
	// resurrecting from remote after recovery. key → 数据/标记（同 key 覆盖）。
	pendingMu      sync.Mutex
	pendingWrites  map[string]pendingWrite
	pendingDeletes map[string]struct{}
	flushing       map[string]struct{} // flush 锁外 IO 期间的 key 占位，防 hasPending 误判驱逐
	lastFlush      time.Time           // 上次 flush 尝试时间戳（受 pendingMu 保护）

	// Probabilistic verification
	verifyEvery  int
	verifyCounts sync.Map // string → *atomic.Int64: access count per key

	// Version sync (background cache coherence)
	versionSyncInterval time.Duration
	versionSyncBatch    int
	versionCursor       string // LRU 采样游标：上一批最后一个 key（空串=从头开始）
	versionStop         chan struct{}

	// Close 幂等保护
	closeOnce sync.Once
	watcherWG sync.WaitGroup // 等待 startWatcher 退出
	closed    atomic.Bool    // Close 后置位，阻止 noticeRemoved 发布
}

// pendingWrite 记录降级期间被跳过的 remote 写入，恢复后补偿写回 remote。
type pendingWrite struct {
	data          []byte
	expireSeconds int
}

// PreLoadFn 是 PreLoad 的加载函数：一次返回全部待预热数据（key → value）。
// 使用方在启动时从数据源加载热点数据调用 PreLoad 写入缓存。
type PreLoadFn func(ctx context.Context) (map[string]any, error)

// LoadFn 是 Getfn 的回源函数。
// "未找到"必须通过返回 (false, nil) 表达：Getfn 缓存空值占位（防穿透）并向
// 调用方返回 ErrEntityNotExist。应用层应在 fn 内自行完成错误语义转换（能明确
// 区分"没找到"与"其他错误"）；返回 error 一律视为真实失败——直接透传给调用方
// 且不写缓存（下次调用重新执行 fn）。
type LoadFn func(ctx context.Context, key string, v any) (bool, error)

// BatchLoadFn 是批量回源函数的契约签名：给定多个 key，从数据源一次性加载
// （如 SQL IN 查询、批量 RPC），返回 key → 值的映射。未加载到的 key 不出现在
// 返回 map 中（调用方按"未找到"处理，可缓存空值占位防穿透）。
//
// 注意：该类型当前为【预留契约】——本版本尚无 API 消费它（GetMulti 未接入
// 批量回源，加载逻辑未实现）。未来实现批量回源功能时使用此签名；外部项目
// 也可按此签名自行实现取数函数。
type BatchLoadFn func(ctx context.Context, keys ...string) (map[string]any, error)

// MutateFn 是 Invalidate 的源变更函数：对数据源执行更新或删除等写操作。
// 返回错误表示源未变更，缓存保持不动。
type MutateFn func(ctx context.Context, key string) error

func New(opts ...Option) *cache {
	c := acquireDefaultCache()

	opt := Options{Name: defaultCacheName}
	for _, o := range opts {
		o(&opt)
	}

	c.localStore = opt.localStore
	c.remoteStore = opt.remoteStore
	c.listener = opt.listener
	// 零参 New（未显式设置任何 store）时默认注入内存缓存，与显式 WithMemStore()
	// 完全一致（同一 newMemStore() 构造、走同一配置循环），消灭"无缓存实例"陷阱。
	// 显式设置过任一 store（storeSet）则不注入——`WithStore(redisStore)` 单独使用
	// 即"只远程"模式（localStore 为 nil，各读写路径已有 nil 保护）。
	if c.localStore == nil && !opt.storeSet {
		c.localStore = newMemStore()
	}
	if opt.Logger != nil {
		c.logger = opt.Logger
	}
	if opt.Cipher != nil {
		c.cipher = opt.Cipher
	}

	opt.init()

	// metrics 必须先于下方 store 配置循环赋值：循环内 ms.metrics = c.metrics 将
	// 驱逐事件回调接线到 mem_store；若晚于此，WithMetrics 注入的实例会被
	// noopMetrics 覆盖，导致驱逐事件丢失。
	if opt.metrics != nil {
		c.metrics = opt.metrics
	}

	// Configure memory stores (eviction, capacity, metrics)
	for _, store := range []Store{c.localStore, c.remoteStore} {
		if ms, ok := store.(*mem_store); ok {
			ms.metrics = c.metrics

			if opt.cleanupInterval > 0 {
				ms.cleanupInterval = opt.cleanupInterval
			}
			if opt.maxItems > 0 {
				ms.maxItems = opt.maxItems
			}
			if opt.maxBytes > 0 {
				ms.maxBytes = opt.maxBytes
			}
			if opt.ttlJitterSet {
				ms.ttlJitter = opt.ttlJitter
			} else {
				// 防雪崩保护默认开启：未显式设置时使用默认 0~30s 抖动
				ms.ttlJitter = defaultTTLJitter
			}
			if opt.slidingWindow > 0 {
				ms.slidingWindow = opt.slidingWindow
			}
			if opt.hotKeyThreshold > 0 {
				ms.hotKeyThreshold = opt.hotKeyThreshold
			}

			ms.startEviction()
		}
	}

	// TTL：仅当显式调用 WithTTL 时覆盖默认值（defaultExpiresSeconds）；
	// WithTTL(<=0) 语义为永不过期（回填/回写经 mem_store.Put(<=0)，
	// mem_store 对 Expiration==0 视为永不过期）。
	if opt.ttlSet {
		c.ttl = opt.TTL
	}

	if opt.degradeThreshold > 0 {
		c.degradeThreshold = opt.degradeThreshold
	}
	if opt.degradeRecovery > 0 {
		c.degradeRecovery = opt.degradeRecovery
	}
	if c.remoteStore != nil && c.degradeThreshold > 0 && c.degradeRecovery > 0 {
		c.degradeStopRecov = make(chan struct{})
		go c.healthLoop()
	}

	if opt.verifyEvery > 0 {
		c.verifyEvery = opt.verifyEvery
	}

	if c.remoteStore != nil && opt.versionSyncInterval > 0 {
		c.versionSyncInterval = opt.versionSyncInterval
		c.versionSyncBatch = defaultVersionSyncBatch
		c.versionStop = make(chan struct{})
		go c.versionSyncLoop()
	}

	c.opt = opt

	c.watcherWG.Add(1)
	go c.startWatcher()

	return c
}

// GetMulti retrieves multiple keys at once. Uses the store's BulkStore
// interface if available, otherwise falls back to individual Get calls.
// Returns a map of deserialized values. Values are deserialized using the
// standard encoding/json library, so they will be the types that json.Unmarshal
// produces (float64 for numbers, string for quoted strings, etc.).
// 并发语义：内部逐 key 走 getFromCache（与 Get 同 key 空间、同一 singleflight
// 合并去重）；与 Getfn 混合并发同一 key 时同样受限（不同 singleflight key，
// 可能各自查询 remote），请统一使用同一种 API。
func (c *cache) GetMulti(ctx context.Context, keys ...string) (map[string]any, error) {
	if len(keys) == 0 {
		return make(map[string]any), nil
	}

	result := make(map[string]any, len(keys))

	// local store 实现 BulkStore 时批量分派（性能路径），
	// 数据携带版本前缀，与单值路径一致做 payloadOf 剥离。
	if bulk, ok := c.localStore.(BulkStore); ok {
		raw, err := bulk.GetMulti(ctx, keys...)
		if err != nil {
			return nil, err
		}
		hit := make(map[string]bool, len(raw))
		for key, data := range raw {
			payload := payloadOf(data)
			if c.isEmptyObject(payload) {
				continue
			}
			// 出口统一解密：store 中为密文，反序列化前还原为明文
			plain, err := c.unseal(payload)
			if err != nil {
				return nil, err
			}
			var v any
			if err := json.Unmarshal(plain, &v); err != nil {
				// Fall back to raw string for serializer-optimized bare strings
				v = string(plain)
			}
			result[key] = v
			hit[key] = true
		}
		// 未命中的 key 回退单值路径（remote fallback / verify / 回写）
		for _, key := range keys {
			if hit[key] {
				continue
			}
			data, exist, err := c.getFromCache(ctx, key, 0)
			if err != nil {
				return nil, err
			}
			if exist && !c.isEmptyObject(data) {
				var v any
				if err := json.Unmarshal(data, &v); err != nil {
					v = string(data)
				}
				result[key] = v
			}
		}
		return result, nil
	}

	for _, key := range keys {
		// Use single Get to properly deserialize through the known-type pathway
		// where possible. For the map[string]any return type, we use json.Unmarshal.
		data, exist, err := c.getFromCache(ctx, key, 0)
		if err != nil {
			return nil, err
		}
		if exist && !c.isEmptyObject(data) {
			var v any
			if err := json.Unmarshal(data, &v); err != nil {
				// Fall back to raw string for serializer-optimized bare strings
				v = string(data)
			}
			result[key] = v
		}
	}

	return result, nil
}

// SetMulti stores multiple key-value pairs. Uses the store's BulkStore
// interface if available, otherwise falls back to individual Put calls.
func (c *cache) SetMulti(ctx context.Context, items map[string]any, expireSeconds int) error {
	if len(items) == 0 {
		return nil
	}

	// Try bulk for each store
	for _, s := range []Store{c.localStore, c.remoteStore} {
		if s == nil {
			continue
		}
		// 降级 + remote 时禁用 bulk：bulk.SetMulti 会绕过 putInStore 的降级
		// pending 缓冲直接写远程，与单键 Put 语义不一致——回退逐 key 路径。
		if bulk, ok := s.(BulkStore); ok && !(c.isDegraded() && s.IsRemote()) {
			dataMap := make(map[string][]byte, len(items))
			for key, val := range items {
				data, err := c.serializer.Marshal(val)
				if err != nil {
					return fmt.Errorf("marshal cache value failed: %w", err)
				}
				sealed, err := c.seal(data)
				if err != nil {
					return err
				}
				// 与单键 Put 路径一致：统一携带版本前缀，
				// 保证 syncBatch 的版本比较在 bulk/单键混用下行为一致。
				dataMap[key] = wrapVersion(sealed)
			}
			if err := bulk.SetMulti(ctx, dataMap, expireSeconds); err != nil {
				if s.IsRemote() {
					// 与单键 Put 路径一致：remote bulk 写失败推进降级计数
					//（recordRemoteError 内部已排除 context.Canceled）。
					// 降级期间本分支已被上方跳过（走 fallback/pending），
					// 此处只覆盖非降级期间的真实 bulk 失败。
					c.recordRemoteError(err)
				}
				return err
			}
			if s.IsRemote() {
				// 与 putInStore 成功路径对称：remote bulk 写成功清零降级计数
				c.recordRemoteSuccess()
			}
		} else {
			if err := c.setMultiFallback(ctx, s, items, expireSeconds); err != nil {
				return err
			}
		}
	}

	return nil
}

// setMultiFallback 逐 key 写入单个 store（非 BulkStore，或降级期间的 remote
// bulk store——经 putInStore 复用降级 pending 缓冲与错误计数）。
func (c *cache) setMultiFallback(ctx context.Context, s Store, items map[string]any, expireSeconds int) error {
	for key, val := range items {
		data, err := c.serializer.Marshal(val)
		if err != nil {
			return fmt.Errorf("marshal cache value failed: %w", err)
		}
		sealed, err := c.seal(data)
		if err != nil {
			return err
		}
		if err := c.putInStore(ctx, s, key, sealed, expireSeconds); err != nil {
			return err
		}
	}
	return nil
}

// DeletePattern deletes all keys matching the given glob pattern.
// Uses the PatternStore interface if available. Does NOT send listener
// notifications (pattern deletes may affect many keys).
func (c *cache) DeletePattern(ctx context.Context, pattern string) {
	for _, s := range []Store{c.localStore, c.remoteStore} {
		if s != nil {
			if ps, ok := s.(PatternStore); ok {
				if err := ps.DeletePattern(ctx, pattern); err != nil {
					c.logger.WarnContext(ctx, "delete pattern from store failed", "pattern", pattern, "store", s.Name(), "err", err)
				}
			}
		}
	}
	// Skip listener notification for pattern deletes (too noisy)
}

// PreLoad 启动预热：从数据源一次性加载热点数据到缓存（全量加载模式）。
// 内部批量写入（SetMulti）：一次序列化/加密/批量写入往返，bulk store 走批量
// 路径、否则逐 key fallback（与逐 key Put 的序列化/加密/版本/降级语义一致）。
// 错误语义与逐 key Put 近似：首个失败即返回（不聚合）。
func (c *cache) PreLoad(ctx context.Context, loadfn PreLoadFn, expirSeconds int) error {
	loaded, err := loadfn(ctx)
	if err != nil {
		return err
	}

	return c.SetMulti(ctx, loaded, expirSeconds)
}

// Get 读取 key 对应的值并反序列化到 v。
// v 的所有权归调用方：实现会通过 serializer.Unmarshal 写入 v 指向的对象。
// 并发语义：同一 key 的并发 Get 由 getFromCache 的 singleflight
// （get-from-cache-%s）合并——一个请求查询（含 remote），其他等待共享结果。
// 限制：同一 key 的并发访问应统一使用同一种 API（Get 或 Getfn）——Get 与
// Getfn 使用不同的 singleflight key，混合并发时可能各自查询 remote，无法合并去重。
func (c *cache) Get(ctx context.Context, key string, v any) error {
	data, exist, err := c.getFromCache(ctx, key, 0)
	if err != nil {
		return err
	}

	return c.respond(data, exist, v)
}

// Getfn 读取 key；本地与 remote 均未命中时调用 fn 回源并缓存。
// 并发语义：同一 key 的并发 Getfn 共享单一 singleflight（key:%s，与 Invalidate 互斥），
// 覆盖「查缓存 → miss → fn 回源 → 回填」全流程——其他线程在 Do 上等待并共享
// 最终结果，不会各自执行 fn。
// v 的所有权归调用方：并发场景下每个调用方应传入独立的 v——
// 多个 goroutine 共享同一 v 时，每个调用方都会独立反序列化到该 v（数据竞争由调用方负责）。
// 限制：Get 与 Getfn 混合并发同一 key 时不共享 singleflight，可能双查 remote；
// 请统一使用同一种 API（Get 或 Getfn）。
// expireSeconds 语义（注意与 Put 的差异）：<=0 使用全局 WithTTL 值（Getfn 免传 TTL）；
// Put 的 0 表示永不过期，两者不同。
func (c *cache) Getfn(ctx context.Context, key string, v any, fn LoadFn, expireSeconds int) error {
	// 免传 TTL：<=0 回落全局 WithTTL 值；归一化后恒 >0，
	// 使 getFromCache 回写与回填 TTL 一致（消除两条路径 TTL 不一致）。
	if expireSeconds <= 0 {
		expireSeconds = c.ttl
	}

	fnkey := fmt.Sprintf("key:%s", key)
	defer c.sg.Forget(fnkey)

	item, err, shared := c.sg.Do(fnkey, func() (interface{}, error) {
		data, exist, err := c.getFromCacheData(ctx, key, expireSeconds)
		if err != nil {
			return storeItem{}, err
		}
		if exist {
			// 出口统一解密：store 中为密文，返回给 respond 前还原为明文
			plain, err := c.unseal(data)
			if err != nil {
				return storeItem{}, err
			}
			return storeItem{bytes: plain, exist: true}, nil
		}

		// miss：只有第一个执行者调用 fn 回源
		c.stats.IncrQuery()
		exist, err = fn(ctx, key, v)
		if err != nil {
			// loadFn 返回任何 error 一律视为真实失败：直接透传、不写占位不缓存
			//（"未找到"须由应用层在 fn 内转换为 (false, nil)）
			c.stats.IncrQueryFail(err)
			return storeItem{}, err
		}

		var d []byte
		if !exist {
			d = c.notExistPlaceholder
		} else {
			d, err = c.serializer.Marshal(v)
			if err != nil {
				// 序列化失败：记录 Warn 并跳过缓存写入，不阻断回源结果返回。
				c.logger.WarnContext(ctx, "marshal loaded value failed, skip caching", "key", key, "err", err)
				return storeItem{bytes: nil, exist: true}, nil
			}
		}

		// Place to cache
		// 回填失败仅记录 Warn（含占位符回填）：不阻断回源结果返回，
		// 否则持续 miss → 每次穿透且零感知。
		if err := c.putCache(ctx, key, d, expireSeconds); err != nil {
			c.logger.WarnContext(ctx, "fill cache failed", "key", key, "err", err)
		}
		return storeItem{bytes: d, exist: exist}, nil
	})

	if shared {
		c.stats.IncrShared()
	}

	if err != nil {
		return err
	}

	// Data loading is complete
	value := item.(storeItem)
	return c.respond(value.bytes, value.exist, v)
}

// Put 写入 key 对应的值。
// expireSecond 语义：<= 0 表示永不过期；> 0 为该条目的 TTL 秒数（有效域
// 1 ~ 9e9 秒，超出可能溢出）。
func (c *cache) Put(ctx context.Context, key string, v any, expireSecond int) error {
	data, err := c.serializer.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal cache value failed: %w", err)
	}

	if err := c.putCache(ctx, key, data, expireSecond); err != nil {
		return err
	}

	return nil
}

// Invalidate 使 key 对应的源数据变更后缓存收敛：mutateFn 内对数据源
// （DB 等）执行更新或删除，成功后自动失效缓存——本地与远程双删、降级期间
// 删除失败进 pending 补偿队列重试、并经 listener 向集群广播，其他实例同步
// 失效本地副本。
//
// 与 Getfn 的回源共享 singleflight 互斥（同一 key 的变更与回填不并发），
// 消除"源已变更、旧值回填"的脏窗口；mutateFn 返回错误时缓存保持不动
// （源未变更）。推荐写路径：凡涉及源数据的写/删一律经本方法包装，
// 由库承包失效与集群收敛，避免业务侧遗漏清理造成缓存不一致。
func (c *cache) Invalidate(ctx context.Context, key string, mutateFn MutateFn) error {
	// 与 Getfn 回源共用同一 singleflight key，使 delete 与 load 互斥。
	fnKey := fmt.Sprintf("key:%s", key)
	defer c.sg.Forget(fnKey)

	_, err, _ := c.sg.Do(fnKey, func() (interface{}, error) {
		err := mutateFn(ctx, key)
		if err != nil {
			return storeItem{}, err
		}

		c.Delete(ctx, key)

		return storeItem{}, nil
	})

	return err
}

func (c *cache) Delete(ctx context.Context, keys ...string) {
	if len(keys) == 0 {
		return // 空参数直接返回，避免向 remote 发送空 Del 引发告警/计数噪音
	}
	c.logger.DebugContext(ctx, "delete cache key", "keys", keys)

	c.removeFromStorage(ctx, c.localStore, keys...)
	c.removeFromStorage(ctx, c.remoteStore, keys...)

	for _, key := range keys {
		c.resetVerifyCount(key)
	}

	c.noticeRemoved(ctx, keys...)
}

func (c *cache) noticeRemoved(ctx context.Context, keys ...string) {
	if c.closed.Load() {
		return
	}
	if c.listener != nil && len(keys) > 0 {
		for _, key := range keys {
			if err := c.listener.Publish(key); err != nil {
				c.logger.WarnContext(ctx, "publish removed key failed", "key", key, "err", err)
			}
		}
	}
}

// Stats 返回一致性只读快照（值拷贝）：外部修改快照不影响内部计数。
func (c *cache) Stats() Stats {
	return c.stats.Snapshot()
}

func (c *cache) Close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.stopChan)
		c.watcherWG.Wait()

		if c.degradeStopRecov != nil {
			close(c.degradeStopRecov)
		}
		if c.versionStop != nil {
			close(c.versionStop)
		}

		if c.localStore != nil {
			if closer, ok := c.localStore.(interface{ Close() }); ok {
				closer.Close()
			}
		}
		if c.remoteStore != nil {
			if closer, ok := c.remoteStore.(interface{ Close() }); ok {
				closer.Close()
			}
		}
	})
}

// shouldVerify returns true if the key's local cache should be checked against
// remote on this access (probabilistic cache coherence). Resets the counter
// after a verification round.
func (c *cache) shouldVerify(key string) bool {
	if c.verifyEvery <= 0 {
		return false
	}

	v, _ := c.verifyCounts.LoadOrStore(key, new(atomic.Int64))
	count := v.(*atomic.Int64)
	val := count.Add(1)
	if val >= int64(c.verifyEvery) {
		count.Store(0)
		return true
	}
	return false
}

// resetVerifyCount removes the access counter for a key (called on Delete).
func (c *cache) resetVerifyCount(key string) {
	if c.verifyEvery > 0 {
		c.verifyCounts.Delete(key)
	}
}

// versionSyncLoop periodically compares local cache entries against the remote
// store. It iterates through local keys in batches (default 100), fetching each
// key from remote and comparing payloads. If data differs → refresh local.
// If key no longer exists remotely → evict local. Skips entirely when degraded.
// This provides pull-based cache coherence independent of PubSub notifications.
func (c *cache) versionSyncLoop() {
	ticker := time.NewTicker(c.versionSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.syncBatch()
		case <-c.versionStop:
			return
		case <-c.stopChan:
			return
		}
	}
}

func (c *cache) syncBatch() {
	if c.isDegraded() {
		return
	}

	ms, ok := c.localStore.(*mem_store)
	if !ok || c.remoteStore == nil {
		return
	}

	// LRU 采样游标：从上一批最后一个 key 之后继续取一批；afterKey 不存在（被驱逐/
	// 首轮回绕）时 SampleKeys 自动从 head 重来。空批代表已扫到链表尾 → 重置游标，
	// 下一轮从头开始（回绕判定由"空批"驱动，取代旧版基于 offset/total 的整数环绕）。
	batch := ms.SampleKeys(c.versionCursor, c.versionSyncBatch)
	if len(batch) == 0 {
		c.versionCursor = ""
		return
	}
	c.versionCursor = batch[len(batch)-1]

	ctx := context.Background()
	for _, key := range batch {
		remoteData, remoteExist, err := c.remoteStore.Get(ctx, key)
		if err != nil {
			continue // transient error, try next cycle
		}

		// 采样读取用 peek（只读、不提升、不扰动 LRU 顺序）：若走 Get 会 moveToFront，
		// 令游标在 head 端簇震荡、tail 端冷 key 长期饿死。此处 localStore 必为
		// *mem_store（上方 !ok 已 return），故直接 peek；若将来放开其他本地 store，
		// 需按 Store 类型回退到其 Get。
		localData, localExist := ms.peek(key)

		lv := versionOf(localData)
		rv := versionOf(remoteData)

		if localExist && !remoteExist {
			// Removed remotely → clear local（等待补偿的 key 不驱逐）
			if c.hasPending(key) {
				c.logger.Warn("version sync skip eviction: key has pending op", "key", key)
				continue
			}
			c.removeFromStorage(ctx, c.localStore, key)
			c.resetVerifyCount(key)
		} else if localExist && remoteExist && rv > lv {
			// Remote has newer data → update local
			// 回写失败仅记录 Warn（sync batch）：持续 miss 会导致每次穿透且零感知
			if err := c.putInStore(ctx, c.localStore, key, payloadOf(remoteData), c.ttl); err != nil {
				c.logger.Warn("sync batch write back to local store failed", "key", key, "err", err)
			}
		}
	}
}

// isDegraded returns true when remote store operations should be skipped.
func (c *cache) isDegraded() bool {
	return c.degraded.Load()
}

// recordRemoteError increments the error counter and enters degraded mode
// when the threshold is reached. context.Canceled（用户主动取消）不计入降级
// 计数——取消不是远程故障；context.DeadlineExceeded 保留计数（超时通常是
// 远程不可用的表现）。
func (c *cache) recordRemoteError(err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	count := c.degradeCount.Add(1)
	if c.degradeThreshold > 0 && count >= int64(c.degradeThreshold) && !c.isDegraded() {
		c.degraded.Store(true)
		c.metrics.SetDegraded(true)
		c.logger.Warn("entering degraded mode: remote store unreachable")
	}
}

// recordRemoteSuccess resets the degrade counter and exits degraded mode.
// 仅在降级刚恢复的转换点立即 flush（force）；其余远程成功由 flushPending 内部
// 的退避间隔控制，避免失败后网络风暴。
func (c *cache) recordRemoteSuccess() {
	wasDegraded := c.isDegraded()
	c.degradeCount.Store(0)
	if wasDegraded {
		c.degraded.Store(false)
		c.metrics.SetDegraded(false)
		c.logger.Warn("exiting degraded mode: remote store recovered")
	}
	c.flushPending(wasDegraded)
}

// healthLoop periodically checks remote store availability and proactively
// enters or exits degraded mode WITHOUT relying on user-request errors.
// This prevents user requests from blocking on network timeouts when
// the remote store is unavailable.
func (c *cache) healthLoop() {
	ticker := time.NewTicker(c.degradeRecovery)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _, err := c.remoteStore.Get(ctx, "__degrade_probe__")
			cancel()

			if err != nil {
				// Remote unavailable → enter degraded immediately
				if !c.isDegraded() {
					c.degraded.Store(true)
					c.metrics.SetDegraded(true)
					c.logger.Warn("health probe failed, entering degraded mode")
				}
			} else {
				// Remote available → exit degraded if was degraded
				if c.isDegraded() {
					c.degraded.Store(false)
					c.metrics.SetDegraded(false)
					c.logger.Warn("health probe succeeded, exiting degraded mode")
					c.flushPending(true)
				}
			}
		case <-c.degradeStopRecov:
			return
		case <-c.stopChan:
			return
		}
	}
}

type storeItem struct {
	bytes []byte
	exist bool
}

// getFromCacheData 读取缓存（本地优先，必要时概率校验 remote），无 singleflight。
// expireSeconds 为请求路径已知的 TTL：Getfn 场景传入调用方指定的 TTL，
// 其它场景（Get/GetMulti）传入 0，回写本地时使用 c.ttl。
// 该实现同时供 getFromCache（Get/GetMulti 的 singleflight 包装）与 Getfn
// （key:%s 全流程 singleflight）复用，保证两路径缓存行为一致。
func (c *cache) getFromCacheData(ctx context.Context, key string, expireSeconds int) ([]byte, bool, error) {
	data, exist, err := c.getFromStore(ctx, c.localStore, key)
	if err != nil {
		return nil, false, err
	}

	// Local hit: if verification is not needed, return immediately.
	// Otherwise fall through to check remote for cache coherence.
	if exist && !c.shouldVerify(key) {
		return data, exist, nil
	}

	localWasStale := exist // true = local had data but we're verifying
	localData := data      // keep local payload for fallback on remote error

	data, exist, err = c.getFromStore(ctx, c.remoteStore, key)
	if err != nil {
		// Remote 出错不代表数据被删除：若本地存在则保留本地数据并返回，
		// 错误仅由 getFromStore 内部计入降级计数，不返回给调用方；
		// 本地也不存在时按错误处理。
		if localWasStale {
			return localData, true, nil
		}
		return nil, false, err
	}

	// 降级期间 remote 不可达（getFromStore 跳过返回 miss）：保留本地数据，
	// 避免把「remote 暂时查不到」误判为「数据已被删除」。
	if c.isDegraded() {
		if localWasStale {
			return localData, true, nil
		}
		return nil, false, nil
	}

	// L1 回写 TTL：优先使用请求路径已知的 TTL（Getfn 传入），否则用 c.ttl。
	ttl := c.ttl
	if expireSeconds > 0 {
		ttl = expireSeconds
	}

	if exist {
		// Remote has the data → sync to local (refresh stale or warm new)
		// 回写失败仅记录 Warn：持续 miss 会导致每次穿透且零感知
		if err := c.putInStore(ctx, c.localStore, key, data, ttl); err != nil {
			c.logger.WarnContext(ctx, "write back to local store failed", "key", key, "err", err)
		}
	} else if localWasStale {
		// Remote doesn't have it → clear stale local entry immediately
		// （等待补偿的 key 不驱逐，恢复后由 flush 补偿）
		if c.hasPending(key) {
			return localData, true, nil
		}
		c.removeFromStorage(ctx, c.localStore, key)
		c.resetVerifyCount(key)
	}

	return data, exist, nil
}

// getFromCache 读取缓存（本地优先，必要时概率校验 remote）。
// 保持 singleflight（get-from-cache-%s）供 Get/GetMulti 路径去重。
func (c *cache) getFromCache(ctx context.Context, key string, expireSeconds int) ([]byte, bool, error) {
	fnKey := fmt.Sprintf("get-from-cache-%s", key)
	defer c.sg.Forget(fnKey)

	ret, err, shared := c.sg.Do(fnKey, func() (interface{}, error) {
		data, exist, err := c.getFromCacheData(ctx, key, expireSeconds)
		if err != nil {
			return storeItem{bytes: nil, exist: false}, err
		}
		// 出口统一解密：store 中为密文，返回给 respond 前还原为明文
		plain, err := c.unseal(data)
		if err != nil {
			return storeItem{bytes: nil, exist: false}, err
		}
		return storeItem{bytes: plain, exist: exist}, nil
	})

	if shared {
		c.stats.IncrShared()
	}

	if d, ok := ret.(storeItem); ok {
		return d.bytes, d.exist, err
	}

	return []byte{}, false, nil
}

// seal 写缓存前（"序列化后首次写入"的统一入口）：无 cipher、空数据或
// 占位符时原样返回，否则 Encrypt。内部搬运路径（verify/syncBatch 回写、
// pending flush）传密文原样，不经过 seal。
func (c *cache) seal(data []byte) ([]byte, error) {
	if c.cipher == nil || len(data) == 0 || c.isEmptyObject(data) {
		return data, nil
	}
	sealed, err := c.cipher.Encrypt(data)
	if err != nil {
		return nil, fmt.Errorf("cipher encrypt failed: %w", err)
	}
	return sealed, nil
}

// unseal 读缓存后（"最终交给 respond/Getfn/GetMulti"的统一出口）：无 cipher、
// 空数据或占位符时原样返回，否则 Decrypt。
func (c *cache) unseal(data []byte) ([]byte, error) {
	if c.cipher == nil || len(data) == 0 || c.isEmptyObject(data) {
		return data, nil
	}
	plain, err := c.cipher.Decrypt(data)
	if err != nil {
		return nil, fmt.Errorf("cipher decrypt failed: %w", err)
	}
	return plain, nil
}

func (c *cache) putCache(ctx context.Context, key string, v []byte, expireSecond int) error {
	// 序列化后首次写入的统一入口：seal 加密后写入两个 store。
	// 内部搬运（verify/syncBatch 回写、flush）走 putInStore 直传密文，不在此列。
	sealed, err := c.seal(v)
	if err != nil {
		return err
	}

	if err := c.putInStore(ctx, c.localStore, key, sealed, expireSecond); err != nil {
		return err
	}

	if err := c.putInStore(ctx, c.remoteStore, key, sealed, expireSecond); err != nil {
		return err
	}

	return nil
}

func (c *cache) getFromStore(ctx context.Context, s Store, key string) ([]byte, bool, error) {
	if s == nil {
		return []byte{}, false, nil
	}

	// Skip remote store when in degraded mode
	if c.isDegraded() && s.IsRemote() {
		return []byte{}, false, nil
	}

	data, exist, err := s.Get(ctx, key)
	if exist {
		c.stats.IncrHit(s.Name())
		// Strip version prefix for user-facing reads
		data = payloadOf(data)
	} else {
		c.stats.IncrMiss(s.Name())
	}

	if err != nil && s.IsRemote() {
		c.recordRemoteError(err)
	} else if err == nil && s.IsRemote() {
		c.recordRemoteSuccess()
	}

	if err != nil {
		if s.IsRemote() && !errors.Is(err, context.Canceled) {
			// 远程故障哨兵包装（保留原错误链），供调用方 errors.Is 区分；
			// context.Canceled 不包装（用户主动取消，非远程故障）。
			return data, exist, fmt.Errorf("%w: store %s get key %s failed: %w", ErrRemoteUnavailable, s.Name(), key, err)
		}
		return data, exist, fmt.Errorf("store %s get key %s failed: %w", s.Name(), key, err)
	}
	return data, exist, nil
}

func (c *cache) removeFromStorage(ctx context.Context, s Store, keys ...string) {
	if s == nil {
		return
	}

	// Skip remote store when in degraded mode; buffer the delete for replay
	// once the remote recovers (prevents deleted data from resurrecting).
	if c.isDegraded() && s.IsRemote() {
		c.pendingMu.Lock()
		if c.pendingDeletes == nil {
			c.pendingDeletes = make(map[string]struct{})
		}
		for _, key := range keys {
			// 删除优先于写：若该 key 有待补偿写，撤销它（移除，不受上限影响），
			// 避免恢复后先写后删
			delete(c.pendingWrites, key)
			if len(c.pendingDeletes) >= maxPendingWrites {
				// 上限：拒绝新删除（恢复后 remote 数据可能复活），Warn 告知
				c.logger.WarnContext(ctx, "pending deletes full, dropping delete", "key", key)
				continue
			}
			c.pendingDeletes[key] = struct{}{}
		}
		c.pendingMu.Unlock()
		return
	}

	if err := s.Delete(ctx, keys...); err != nil {
		if s.IsRemote() {
			c.recordRemoteError(err)
		}
		c.logger.WarnContext(ctx, "delete from store failed", "store", s.Name(), "keys", keys, "err", err)
	} else if s.IsRemote() {
		c.recordRemoteSuccess()
	}
}

func (c *cache) putInStore(ctx context.Context, s Store, key string, b []byte, expireSecond int) error {
	if s == nil {
		return nil
	}

	// Skip remote store when in degraded mode; buffer the write for replay
	// once the remote recovers (prevents local-only data from being evicted
	// by version sync / verify after recovery).
	if c.isDegraded() && s.IsRemote() {
		c.pendingMu.Lock()
		if c.pendingWrites == nil {
			c.pendingWrites = make(map[string]pendingWrite)
		}
		if len(c.pendingWrites) >= maxPendingWrites {
			c.pendingMu.Unlock()
			return fmt.Errorf("%w: key %s", ErrPendingWritesFull, key)
		}
		// 写入优先于待删标记：数据已更新，撤销 pending 删除
		delete(c.pendingDeletes, key)
		c.pendingWrites[key] = pendingWrite{data: b, expireSeconds: expireSecond}
		c.pendingMu.Unlock()
		return nil
	}

	// Wrap with timestamp for time-based cache coherence
	if err := s.Put(ctx, key, wrapVersion(b), expireSecond); err != nil {
		if s.IsRemote() {
			c.recordRemoteError(err)
			if !errors.Is(err, context.Canceled) {
				// 远程故障哨兵包装（保留原错误链）
				return fmt.Errorf("%w: store %s put key %s failed: %w", ErrRemoteUnavailable, s.Name(), key, err)
			}
		}
		return fmt.Errorf("store %s put key %s failed: %w", s.Name(), key, err)
	}
	if s.IsRemote() {
		c.recordRemoteSuccess()
	}
	return nil
}

// flushPending 把缓冲的待删/待写操作补偿回写 remote。
// force=true（降级刚恢复的转换点）忽略退避立即执行；否则受 flushRetryInterval
// 退避控制。失败保留在 pending 中由后续触发点重试。
// 锁内仅摘取快照，网络 IO 在锁外执行（带 flushTimeout 超时）。
func (c *cache) flushPending(force bool) {
	if c.remoteStore == nil {
		return
	}

	c.pendingMu.Lock()
	if !force && time.Since(c.lastFlush) < flushRetryInterval {
		c.pendingMu.Unlock()
		return
	}
	c.lastFlush = time.Now()
	deletes := c.pendingDeletes
	writes := c.pendingWrites
	c.pendingDeletes = nil
	c.pendingWrites = nil
	// 摘取后保留 key 占位供 hasPending 查询：flush 锁外 IO（最长 flushTimeout）
	// 期间 syncBatch/verify 不得把该 key 误判为"remote 已删"而驱逐 local。
	// flushing 大小受 pending 上限约束（瞬态、有界），无需额外上限。
	if c.flushing == nil {
		c.flushing = make(map[string]struct{})
	}
	for k := range deletes {
		c.flushing[k] = struct{}{}
	}
	for k := range writes {
		c.flushing[k] = struct{}{}
	}
	c.pendingMu.Unlock()

	if len(deletes) == 0 && len(writes) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()

	var firstErr error
	for key := range deletes {
		err := c.remoteStore.Delete(ctx, key)
		c.pendingMu.Lock()
		if err != nil {
			c.logger.Warn("flush pending delete failed, keeping for retry", "key", key, "err", err)
			if c.pendingDeletes == nil {
				c.pendingDeletes = make(map[string]struct{})
			}
			c.pendingDeletes[key] = struct{}{}
			if firstErr == nil {
				firstErr = err
			}
		}
		// 该 key 处理完成：移除 flushing 占位（失败已回填 pending，无保护窗口）
		delete(c.flushing, key)
		c.pendingMu.Unlock()
	}

	for key, pw := range writes {
		err := c.remoteStore.Put(ctx, key, wrapVersion(pw.data), pw.expireSeconds)
		c.pendingMu.Lock()
		if err != nil {
			c.logger.Warn("flush pending write to remote failed, keeping for retry", "key", key, "err", err)
			if c.pendingWrites == nil {
				c.pendingWrites = make(map[string]pendingWrite)
			}
			c.pendingWrites[key] = pw
			if firstErr == nil {
				firstErr = err
			}
		}
		delete(c.flushing, key)
		c.pendingMu.Unlock()
	}

	// flush 有失败说明 remote 仍不可达 → 推进降级计数
	if firstErr != nil {
		c.recordRemoteError(firstErr)
	}
}

// hasPending 返回 key 是否仍在等待补偿（写/删/flush 进行中）——期间不得被
// 版本同步/校验驱逐。
func (c *cache) hasPending(key string) bool {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if _, ok := c.pendingWrites[key]; ok {
		return true
	}
	if _, ok := c.pendingDeletes[key]; ok {
		return true
	}
	_, ok := c.flushing[key]
	return ok
}

func (c *cache) startWatcher() {
	defer c.watcherWG.Done()

	if c.listener != nil {
		ch := c.listener.Subscribe()
		for {
			select {
			case key, ok := <-ch:
				if !ok {
					_ = c.listener.Close(context.Background())
					return
				}
				c.removeFromStorage(context.Background(), c.localStore, key)
			case <-c.stopChan:
				_ = c.listener.Close(context.Background())
				return
			}
		}
	}
}

func (c *cache) respond(data []byte, exist bool, v any) error {
	if exist && !c.isEmptyObject(data) {
		if err := c.serializer.Unmarshal(data, v); err != nil {
			return fmt.Errorf("unmarshal cache value failed: %w", err)
		}

		return nil
	}

	return ErrEntityNotExist
}

func (c *cache) isEmptyObject(data []byte) bool {
	return bytes.Equal(data, c.notExistPlaceholder)
}

// --- Version wrapper for time-based cache coherence ---

// wrapVersion prepends a magic marker + 8-byte timestamp to the data.
// Uses 0xFB as marker byte — JSON/UTF-8 data never starts with this byte,
// so old cached data without a version prefix is always left intact.
func wrapVersion(data []byte) []byte {
	buf := make([]byte, versionPrefixLen+len(data))
	buf[0] = versionMarker
	binary.BigEndian.PutUint64(buf[1:versionPrefixLen], uint64(time.Now().UnixMilli()))
	copy(buf[versionPrefixLen:], data)
	return buf
}

// versionOf extracts the timestamp from version-wrapped data.
// Returns 0 if the data does not have a version prefix (old format).
func versionOf(data []byte) int64 {
	if len(data) < versionPrefixLen || data[0] != versionMarker {
		return 0
	}
	return int64(binary.BigEndian.Uint64(data[1:versionPrefixLen]))
}

// payloadOf strips the version prefix from data.
// Returns the original data if no prefix is present.
func payloadOf(data []byte) []byte {
	if len(data) < versionPrefixLen || data[0] != versionMarker {
		return data
	}
	return data[versionPrefixLen:]
}

func acquireDefaultCache() *cache {
	return &cache{
		notExistPlaceholder: []byte(defaultNotExistPlaceholder),
		serializer:          jsonSerializer{},
		// slog.Default() 在构造时快照当前默认日志器实例：请在完成 logger
		// 初始化/替换（如 slog.SetDefault）之后再构造 cache，否则将捕获到旧默认实例。
		logger:           slog.Default().With("module", "cache"),
		sg:               singleflight.Group{},
		stats:            newStats(),
		metrics:          noopMetrics{},
		ttl:              defaultExpiresSeconds,
		stopChan:         make(chan struct{}),
		degradeThreshold: 3,
		degradeRecovery:  5 * time.Second,
	}
}
