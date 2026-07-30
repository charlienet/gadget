package cache

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"git.charlienet.top/go/gadget/logger"
	"golang.org/x/sync/singleflight"
)

const (
	versionMarker             = 0xFB // magic byte prefix to distinguish versioned data
	versionPrefixLen          = 9    // 1 magic byte + 8-byte unix millisecond timestamp
	defaultVersionSyncInterval = 30 * time.Second
	defaultVersionSyncBatch    = 100
)

const (
	defaultCacheName           = "cache"
	defaultNotExistPlaceholder = "*"
	defaultExpiresSeconds      = 60
)

var (
	ErrEntityNotExist = errors.New("entity does not exist")
)

type Cache interface {
	Get(ctx context.Context, key string, v any) error
	Getfn(ctx context.Context, key string, v any, fn LoadFn, expireSeconds int) error
	Put(ctx context.Context, key string, v any, expireSecond int) error
	Delete(ctx context.Context, keys ...string)
	Close()
}

type cache struct {
	localStore          Store         // 堆缓存
	remoteStore         Store         // 远程缓存
	listener            Listener      // 异步消息通知
	serializer          Serializer    // 序列化
	notExistPlaceholder []byte        // 缓存击穿空对象
	logger              logger.Logger // 日志
	opt                 Options
	sg                  singleflight.Group
	stats               Stats
	metrics             Metrics
	ttl                 int
	stopChan            chan struct{}

	// Degraded mode
	degraded          atomic.Bool
	degradeCount      atomic.Int64
	degradeThreshold  int
	degradeRecovery   time.Duration
	degradeStopRecov  chan struct{}

	// Probabilistic verification
	verifyEvery   int
	verifyCounts  sync.Map // string → *atomic.Int64: access count per key

	// Version sync (background cache coherence)
	versionSyncInterval time.Duration
	versionSyncBatch    int
	versionCursor       int
	versionStop         chan struct{}
}

type PreLoadFn func(ctx context.Context) (map[string]any, error)
type LoadFn func(ctx context.Context, key string, v any) (bool, error)
type BatchLoadFn func(ctx context.Context, keys ...string) (map[string]any, error)
type UpdateFn func(ctx context.Context, key string) error

func New(opts ...Option) *cache {
	c := acquireDefaultCache()

	opt := Options{Name: defaultCacheName}
	for _, o := range opts {
		o(&opt)
	}

	c.localStore = opt.localStore
	c.remoteStore = opt.remoteStore
	c.listener = opt.listener

	opt.init()

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
			if opt.ttlJitter > 0 {
				ms.ttlJitter = opt.ttlJitter
			}
			if opt.slidingWindow > 0 {
				ms.slidingWindow = opt.slidingWindow
			}

			ms.startEviction()
		}
	}

	if opt.TTL > 0 {
		c.ttl = opt.TTL
	}

	if opt.metrics != nil {
		c.metrics = opt.metrics
	}

	if opt.degradeThreshold > 0 {
		c.degradeThreshold = opt.degradeThreshold
	}
	if opt.degradeRecovery > 0 {
		c.degradeRecovery = opt.degradeRecovery
	}
	if c.remoteStore != nil && c.degradeThreshold > 0 {
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

	go c.startWatcher()

	return c
}


// GetMulti retrieves multiple keys at once. Uses the store's BulkStore
// interface if available, otherwise falls back to individual Get calls.
// Returns a map of deserialized values. Values are deserialized using the
// standard encoding/json library, so they will be the types that json.Unmarshal
// produces (float64 for numbers, string for quoted strings, etc.).
func (c *cache) GetMulti(ctx context.Context, keys ...string) (map[string]any, error) {
	if len(keys) == 0 {
		return make(map[string]any), nil
	}

	result := make(map[string]any, len(keys))

	for _, key := range keys {
		// Use single Get to properly deserialize through the known-type pathway
		// where possible. For the map[string]any return type, we use json.Unmarshal.
		data, exist, err := c.getFromCache(ctx, key)
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
		if bulk, ok := s.(BulkStore); ok {
			dataMap := make(map[string][]byte, len(items))
			for key, val := range items {
				data, err := c.serializer.Marshal(val)
				if err != nil {
					return err
				}
				dataMap[key] = data
			}
			if err := bulk.SetMulti(ctx, dataMap, expireSeconds); err != nil {
				return err
			}
		} else {
			for key, val := range items {
				data, err := c.serializer.Marshal(val)
				if err != nil {
					return err
				}
				if err := c.putInStore(ctx, s, key, data, expireSeconds); err != nil {
					return err
				}
			}
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
				ps.DeletePattern(ctx, pattern)
			}
		}
	}
	// Skip listener notification for pattern deletes (too noisy)
}

func (c *cache) PreLoad(ctx context.Context, loadfn PreLoadFn, expirSeconds int) error {
	loaded, err := loadfn(ctx)
	if err != nil {
		return err
	}

	for k, v := range loaded {
		if err := c.Put(ctx, k, v, expirSeconds); err != nil {
			return err
		}
	}

	return nil
}

func (c *cache) Get(ctx context.Context, key string, v any) error {
	data, exist, err := c.getFromCache(ctx, key)
	if err != nil {
		return err
	}

	return c.respond(data, exist, v)
}

func (c *cache) Getfn(ctx context.Context, key string, v any, fn LoadFn, expireSeconds int) error {
	// Load data from the cache
	data, exist, err := c.getFromCache(ctx, key)
	if err != nil {
		return err
	}

	// not in cache
	if !exist {
		// Load from data source, and cache.
		return c.getFromSource(ctx, key, fn, v, expireSeconds)
	}

	return c.respond(data, exist, v)
}

func (c *cache) Put(ctx context.Context, key string, v any, expireSecond int) error {
	data, err := c.serializer.Marshal(v)
	if err != nil {
		return err
	}

	if err := c.putCache(ctx, key, data, expireSecond); err != nil {
		return err
	}

	return nil
}

func (c *cache) Update(ctx context.Context, key string, updateFn UpdateFn) error {
	fnKey := fmt.Sprintf("update_%s", key)
	defer c.sg.Forget(fnKey)

	_, err, _ := c.sg.Do(fnKey, func() (interface{}, error) {
		err := updateFn(ctx, key)
		if err != nil {
			return nil, err
		}

		c.Delete(ctx, key)

		return nil, err
	})

	return err
}

func (c *cache) Delete(ctx context.Context, keys ...string) {
	c.logger.Debug("delete cache key:", keys)

	c.removeFromStorage(ctx, c.localStore, keys...)
	c.removeFromStorage(ctx, c.remoteStore, keys...)

	for _, key := range keys {
		c.resetVerifyCount(key)
	}

	c.noticeRemoved(keys...)
}

func (c *cache) noticeRemoved(keys ...string) {
	if c.listener != nil && len(keys) > 0 {
		for _, key := range keys {
			c.listener.Publish(key)
		}
	}
}

func (c *cache) Stats() *Stats {
	return &c.stats
}

func (c *cache) Close() {
	close(c.stopChan)

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

	total := ms.Len()
	if total == 0 {
		c.versionCursor = 0
		return
	}

	// Wrap cursor to prevent unbounded growth
	if c.versionCursor >= total {
		c.versionCursor = 0
	}

	batch := ms.SampleKeys(c.versionCursor, c.versionSyncBatch)
	c.versionCursor += len(batch)

	ctx := context.Background()
	for _, key := range batch {
		remoteData, remoteExist, err := c.remoteStore.Get(ctx, key)
		if err != nil {
			continue // transient error, try next cycle
		}

		localData, localExist, _ := c.localStore.Get(ctx, key)

		lv := versionOf(localData)
		rv := versionOf(remoteData)

		if localExist && !remoteExist {
			// Removed remotely → clear local
			c.logger.Debugf("version sync: evicting stale key:[%s]", key)
			c.removeFromStorage(ctx, c.localStore, key)
			c.resetVerifyCount(key)
		} else if localExist && remoteExist && rv > lv {
			// Remote has newer data → update local
			c.logger.Debugf("version sync: refreshing key:[%s] local=%d remote=%d", key, lv, rv)
			c.putInStore(ctx, c.localStore, key, payloadOf(remoteData), c.ttl)
		}
	}
}

// isDegraded returns true when remote store operations should be skipped.
func (c *cache) isDegraded() bool {
	return c.degraded.Load()
}

// recordRemoteError increments the error counter and enters degraded mode
// when the threshold is reached.
func (c *cache) recordRemoteError() {
	count := c.degradeCount.Add(1)
	if c.degradeThreshold > 0 && count >= int64(c.degradeThreshold) && !c.isDegraded() {
		c.degraded.Store(true)
		c.metrics.SetDegraded(true)
		c.logger.Warn("entering degraded mode: remote store unreachable")
	}
}

// recordRemoteSuccess resets the degrade counter and exits degraded mode.
func (c *cache) recordRemoteSuccess() {
	c.degradeCount.Store(0)
	if c.isDegraded() {
		c.degraded.Store(false)
		c.metrics.SetDegraded(false)
		c.logger.Warn("exiting degraded mode: remote store recovered")
	}
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

func (c *cache) getFromCache(ctx context.Context, key string) ([]byte, bool, error) {
	fnKey := fmt.Sprintf("get-from-cache-%s", key)
	defer c.sg.Forget(fnKey)

	ret, err, shared := c.sg.Do(fnKey, func() (interface{}, error) {
		data, exist, err := c.getFromStore(ctx, c.localStore, key)
		if err != nil {
			return nil, err
		}

		// Local hit: if verification is not needed, return immediately.
		// Otherwise fall through to check remote for cache coherence.
		if exist && !c.shouldVerify(key) {
			return storeItem{bytes: data, exist: exist}, nil
		}

		localWasStale := exist // true = local had data but we're verifying

		data, exist, err = c.getFromStore(ctx, c.remoteStore, key)

		if exist {
			// Remote has the data → sync to local (refresh stale or warm new)
			c.putInStore(ctx, c.localStore, key, data, c.ttl)
		} else if localWasStale {
			// Remote doesn't have it → clear stale local entry immediately
			c.logger.Debugf("probabilistic verify: clearing stale local key:[%s]", key)
			c.removeFromStorage(ctx, c.localStore, key)
			c.resetVerifyCount(key)
		}

		return storeItem{bytes: data, exist: exist}, err
	})

	if shared {
		c.stats.IncrShared()
	}

	if d, ok := ret.(storeItem); ok {
		return d.bytes, d.exist, err
	}

	return []byte{}, false, nil
}

func (c *cache) getFromSource(ctx context.Context, key string, loadFn LoadFn, v any, expireSeconds int) error {
	fnkey := fmt.Sprintf("get_from_source_%s", key)
	defer c.sg.Forget(fnkey)

	item, err, shared := c.sg.Do(fnkey, func() (interface{}, error) {
		c.stats.IncrQuery()

		exist, err := loadFn(ctx, key, v)
		if err != nil {
			c.stats.IncrQueryFail(err)
			return nil, err
		}

		var data []byte
		if !exist {
			data = c.notExistPlaceholder
		} else {
			data, _ = c.serializer.Marshal(v)
		}

		// Place to cache
		c.putCache(ctx, key, data, expireSeconds)
		return storeItem{bytes: data, exist: exist}, nil
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

func (c *cache) putCache(ctx context.Context, key string, v []byte, expireSecond int) error {
	if err := c.putInStore(ctx, c.localStore, key, v, expireSecond); err != nil {
		return err
	}

	if err := c.putInStore(ctx, c.remoteStore, key, v, expireSecond); err != nil {
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
		c.logger.Debugf("degraded mode, skip remote store: %s", s.Name())
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
		c.recordRemoteError()
	} else if err == nil && s.IsRemote() {
		c.recordRemoteSuccess()
	}

	c.logger.Debugf("get data from: %s, key:[%s] %v", s.Name(), key, exist)
	return data, exist, err
}

func (c *cache) removeFromStorage(ctx context.Context, s Store, keys ...string) {
	if s == nil {
		return
	}

	// Skip remote store when in degraded mode
	if c.isDegraded() && s.IsRemote() {
		return
	}

	s.Delete(ctx, keys...)
}

func (c *cache) putInStore(ctx context.Context, s Store, key string, b []byte, expireSecond int) error {
	if s == nil {
		return nil
	}

	// Skip remote store when in degraded mode
	if c.isDegraded() && s.IsRemote() {
		return nil
	}

	c.logger.Debugf("cache data to: %s cache key:[%s]", s.Name(), key)

	// Wrap with timestamp for time-based cache coherence
	return s.Put(ctx, key, wrapVersion(b), expireSecond)
}

func (c *cache) startWatcher() {
	if c.listener != nil {
		ch := c.listener.Subscribe()
		for {
			select {
			case key := <-ch:
				c.removeFromStorage(context.Background(), c.localStore, key)
			case <-c.stopChan:
				c.logger.Debug("cache is close")
				c.listener.Close()
				return
			}
		}
	}
}

func (c *cache) respond(data []byte, exist bool, v any) error {
	if exist && !c.isEmptyObject(data) {
		if err := c.serializer.Unmarshal(data, v); err != nil {
			return err
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
		logger:              logger.DefaultLogger,
		sg:                  singleflight.Group{},
		stats:               newStats(),
		metrics:             noopMetrics{},
		ttl:                 defaultExpiresSeconds,
		stopChan:            make(chan struct{}),
		degradeThreshold:    3,
		degradeRecovery:     5 * time.Second,
	}
}
