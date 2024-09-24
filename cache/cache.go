package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/charlienet/gadget/logger"
	"golang.org/x/sync/singleflight"
)

const (
	defaultCacheName           = "cache"
	defaultNotExistPlaceholder = "*"
	defaultExpiresSeconds      = 60
	defaultMaxRetry            = 2
)

var (
	ErrEntityNotExist = errors.New("entity does not exist")
	ErrNotExist       = errors.New("key not exist")
	ErrTimeout        = errors.New("load from source timeout")
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
	ttl                 int
	maxRetry            int
	disable             bool
	stopChan            chan struct{}
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
	c.opt = opt

	go c.startWatcher()

	return c
}

func (c *cache) init() {
	if i, ok := c.localStore.(interface{ Initialize(Options) }); ok {
		i.Initialize(c.opt)
	}
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
	if c.disable {
		return c.getFromSource(ctx, key, fn, v, expireSeconds)
	}

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

		if exist {
			return storeItem{bytes: data, exist: exist}, nil
		}

		data, exist, err = c.getFromStore(ctx, c.remoteStore, key)

		// exists in remote storage
		if exist {
			// Synchronize the remote cache to the local cache
			c.putInStore(ctx, c.localStore, key, data, c.ttl)
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

	c.stats.IncrQuery()
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
	if s != nil {
		data, exist, err := s.Get(ctx, key)
		if exist {
			c.stats.IncrHit(s.Name())
		} else {
			c.stats.IncrMiss(s.Name())
		}
		c.logger.Debugf("get data from: %s, key:[%s] %v", s.Name(), key, exist)

		return data, exist, err
	}

	return []byte{}, false, nil
}

func (c *cache) removeFromStorage(ctx context.Context, s Store, keys ...string) {
	if s != nil {
		s.Delete(ctx, keys...)
	}
}

func (c *cache) putInStore(ctx context.Context, s Store, key string, b []byte, expireSecond int) error {
	if s != nil {
		c.logger.Debugf("cache data to: %s cache key:[%s]", s.Name(), key)
		return s.Put(ctx, key, b, expireSecond)
	}

	return nil
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

func acquireDefaultCache() *cache {
	return &cache{
		notExistPlaceholder: []byte(defaultNotExistPlaceholder),
		serializer:          jsonSerializer{},
		logger:              logger.DefaultLogger,
		sg:                  singleflight.Group{},
		stats:               newStats(),
		ttl:                 defaultExpiresSeconds,
		maxRetry:            defaultMaxRetry,
		stopChan:            make(chan struct{}),
	}
}
