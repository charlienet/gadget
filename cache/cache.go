package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/charlienet/gadget/logger"
	"github.com/charlienet/go-misc/locker"
)

const (
	defaultObjectNotExist = "OBJECT-DOES-NOT-EXIST"
	defaultExpiresSeconds = 60
	defaultMaxRetry       = 2
)

var (
	ErrEntityNotExist = errors.New("entity does not exist")
	ErrTimeout        = errors.New("load from source timeout")
)

type Cache interface {
	BatchGet(ctx context.Context, keys []string, loadFn BatchLoadFn, expirSecond int) error
	Get(ctx context.Context, key string, v any) error
	Getfn(ctx context.Context, key string, v any, fn LoadFn, expireSeconds int) error
	Put(ctx context.Context, key string, v any, expireSecond int) error
	Delete(ctx context.Context, keys ...string)
	Disable()
	Enable()
}

type cache struct {
	localStore       Store                    // 堆缓存
	remoteStore      Store                    // 远程缓存
	pubsub           Listener                 // 异步消息通知
	serializer       Serializer               // 序列化
	emptyObjectToken []byte                   // 缓存击穿空对象
	logger           logger.Logger            // 日志
	locker           *locker.ChanSourceLocker // 资源锁
	qps              *qps
	stats            Stats
	ttl              int
	maxRetry         int
	disable          bool
	stop             chan bool
}

type LoadFn func(ctx context.Context, key string, v any) (bool, error)
type BatchLoadFn func(ctx context.Context, keys ...string) (map[string]any, error)
type UpdateFn func(ctx context.Context, key string) error

func New(opts ...Option) *cache {
	c := acquireDefaultCache()

	opt := Options{}
	for _, o := range opts {
		o(&opt)
	}

	c.localStore = opt.localStore
	c.remoteStore = opt.remoteStore

	c.startWatcher()

	return c
}

func (c *cache) BatchGet(ctx context.Context, keys []string, loadFn BatchLoadFn, expirSecond int) error {
	c.lockMultiple(keys...)
	defer c.unlockMultiple(keys...)

	loaded, err := loadFn(ctx, keys...)
	if err != nil {
		return err
	}

	for key, value := range loaded {
		if err := c.Put(ctx, key, value, expirSecond); err != nil {
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

	if exist && !c.isEmptyObject(data) {
		return c.serializer.Unmarshal(data, &v)
	}

	return ErrEntityNotExist
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

	// Exists and is not an empty object
	if exist && !c.isEmptyObject(data) {
		return c.serializer.Unmarshal(data, v)
	}

	// Load from data source
	return c.getFromSource(ctx, key, fn, v, expireSeconds)
}

func (c *cache) Put(ctx context.Context, key string, v any, expireSecond int) error {
	b, err := c.serializer.Marshal(v)
	if err != nil {
		return err
	}

	if err := c.setCache(ctx, key, b, expireSecond); err != nil {
		return err
	}

	return nil
}

func (c *cache) Update(ctx context.Context, key string, updateFn UpdateFn) error {
	c.locker.Lock(key)
	defer c.locker.Unlock(key)

	return updateFn(ctx, key)
}

func (c *cache) Delete(ctx context.Context, keys ...string) {
	c.lockMultiple(keys...)
	defer c.unlockMultiple(keys...)

	c.logger.Debug("delete cache key:", keys)
	c.localStore.Delete(ctx, keys...)
	c.remoteStore.Delete(ctx, keys...)

	c.noticeHasBeenRemoved(keys...)
}

func (c *cache) lockMultiple(keys ...string) {
	for _, k := range keys {
		c.locker.Lock(k)
	}
}

func (c *cache) unlockMultiple(keys ...string) {
	for _, k := range keys {
		c.locker.Unlock(k)
	}
}

func (c *cache) noticeHasBeenRemoved(keys ...string) {
	if c.pubsub != nil && len(keys) > 0 {
		for _, key := range keys {
			c.pubsub.Publish(key)
		}
	}
}

func (c *cache) Stats() Stats {
	return c.stats
}

func (c *cache) Disable() {
	c.disable = true
}

func (c *cache) Enable() {
	c.disable = false
}

func (c *cache) getFromCache(ctx context.Context, key string) (b []byte, exist bool, err error) {
	b, exist, err = c.getFromStore(ctx, c.localStore, key)
	if err != nil {
		return []byte{}, false, err
	}

	// exists in the local cache, returned
	if exist {
		return
	}

	b, exist, err = c.getFromStore(ctx, c.remoteStore, key)
	if err != nil {
		return
	}

	// exists in remote storage
	if exist {
		// put the data from the remote cache locally
		c.putInStore(ctx, c.localStore, key, b, c.ttl)
	}

	return
}

func (c *cache) getFromSource(ctx context.Context, key string, loadFn LoadFn, v any, expireSeconds int) error {
	ok, ch := c.locker.Lock(key)
	if ok {
		defer c.locker.Unlock(key)

		exist, err := loadFn(ctx, key, v)
		c.logger.Debug("load from source:", exist, err)

		if err != nil {
			return err
		}

		// 异步方式放入
		if exist {
			c.Put(ctx, key, v, expireSeconds)
		} else {
			c.Put(ctx, key, c.emptyObjectToken, expireSeconds)
		}

		return nil
	}

	<-ch
	b, exist, err := c.getFromStore(ctx, c.localStore, key)
	if err != nil {
		return err
	}

	if exist && !c.isEmptyObject(b) {
		return json.Unmarshal(b, &v)
	}

	return ErrEntityNotExist
}

func (c *cache) deleteLocalCache(ctx context.Context, key string) {
	if c.localStore != nil {
		c.localStore.Delete(ctx, key)
	}
}

func (c *cache) setCache(ctx context.Context, key string, v []byte, expireSecond int) error {
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
		c.logger.Debugf("get data from: %s, key:[%s]", s.Name(), key)
		return s.Get(ctx, key)
	}

	return []byte{}, false, nil
}

func (c *cache) putInStore(ctx context.Context, s Store, key string, b []byte, expireSecond int) error {
	if s != nil {
		c.logger.Debugf("cache data to: %s cache key:[%s]", s.Name(), key)
		return s.Put(ctx, key, b, expireSecond)
	}

	return nil
}

func (c *cache) startWatcher() {
	if c.pubsub != nil {
		c.pubsub.Subscribe("notice")

		cc := make(chan string)
		for {
			select {
			case key := <-cc:
				c.deleteLocalCache(context.Background(), key)
			case <-c.stop:
				return
			}
		}
	}
}

func (c *cache) isEmptyObject(data []byte) bool {
	return len(data) == len(c.emptyObjectToken) && bytes.Equal(data, c.emptyObjectToken)
}

func acquireDefaultCache() *cache {
	return &cache{
		emptyObjectToken: []byte(defaultObjectNotExist),
		serializer:       jsonSerializer{},
		logger:           logger.DefaultLogger,
		locker:           locker.NewChanSourceLocker(),
		qps:              &qps{},
		ttl:              defaultExpiresSeconds,
		maxRetry:         defaultMaxRetry,
		stop:             make(chan bool),
	}
}
