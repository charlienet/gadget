package cache

import (
	"bytes"
	"context"
	"errors"
	"log"

	"github.com/charlienet/go-misc/locker"
)

const (
	defaultRedisEmpty     = "object-empty"
	defaultRedisTtlFactor = 20
	defaultMaxRetry       = 2
)

var (
	ErrNotFound       = errors.New("not found")
	ErrEntityNotExist = errors.New("entity does not exist")
	ErrTimeout        = errors.New("load from source timeout")
)

// Store is the interface that wraps the cache store.
type Store interface {
	// Get gets a cached value by key.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Put stores a key-value pair into cache.
	Put(ctx context.Context, key string, v []byte, expireSecond int) error
	// Delete removes a key from cache.
	Delete(ctx context.Context, key ...string) error
	// String returns the name of the implementation.
	Name() string
	//  is remote storage
	IsRemote() bool
}

type PubSubChannel interface {
	Subscribe(key string)
	Publish(key string) error
}

type Serializer interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(b []byte, v any) error
}

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
	local            Store
	remote           Store
	stores           []Store
	pubsub           PubSubChannel
	serializer       Serializer
	emptyObjectToken []byte
	lock             *locker.ChanSourceLocker
	qps              *qps
	stats            Stats
	maxRetry         int
	disable          bool
}

type LoadFn func(ctx context.Context, key string, v any) (bool, error)
type BatchLoadFn func(ctx context.Context, keys ...string) (map[string]any, error)

func New(opts ...Option) *cache {
	c := acquireDefaultCache()

	opt := Options{}
	for _, o := range opts {
		o(&opt)
	}

	c.stores = opt.stores
	for _, s := range opt.stores {
		if s.IsRemote() {
			c.remote = s
		} else {
			c.local = s
		}
	}

	c.startWatcher()

	return c
}

func (c *cache) BatchGet(ctx context.Context, keys []string, loadFn BatchLoadFn, expirSecond int) error {
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

	if exist {
		if err := c.serializer.Unmarshal(data, &v); err != nil {
			return err
		}

		return nil
	}

	return ErrEntityNotExist
}

func (c *cache) Getfn(ctx context.Context, key string, v any, fn LoadFn, expireSeconds int) error {
	if c.disable {
		return c.getFromSource(ctx, key, fn, v, expireSeconds)
	}

	data, exist, err := c.getFromCache(ctx, key)
	if err != nil {
		return err
	}

	if c.isEmpty(data) {
		return ErrEntityNotExist
	}

	if exist {
		err := c.serializer.Unmarshal(data, v)
		return err
	}

	sourceExist, err := fn(ctx, key, v)
	if err != nil {
		return err
	}

	if !sourceExist {
		c.Put(ctx, key, c.emptyObjectToken, expireSeconds)
	} else {
		c.Put(ctx, key, v, expireSeconds)
	}

	return nil
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

func (c *cache) Delete(ctx context.Context, keys ...string) {
	for _, c := range c.stores {
		c.Delete(ctx, keys...)
	}

	log.Println("delete cache key:", keys)

	if c.pubsub != nil {
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

func (c *cache) getFromCache(ctx context.Context, key string) ([]byte, bool, error) {
	for _, s := range c.stores {
		log.Printf("get the value from the cache: %s", s.Name())
		data, exist, err := s.Get(ctx, key)
		if err != nil {
			return []byte{}, false, err
		}

		if exist {
			return data, exist, nil
		}
	}

	return []byte{}, false, nil
}

func (c *cache) getFromSource(ctx context.Context, key string, loadFn LoadFn, v any, expireSeconds int) error {
	replyNum := 1
	_ = replyNum

	owner, ch := c.lock.Lock(key)
	if owner {
		defer c.lock.Unlock(key)

		exist, err := loadFn(ctx, key, v)
		log.Println("load from source:", exist, err)
		if err != nil {
			return err
		}

		if exist {
			c.Put(ctx, key, v, expireSeconds)
		} else {
			c.Put(ctx, key, c.emptyObjectToken, expireSeconds)
		}
	}

	<-ch
	return nil

	// select {
	// case <-ch:
	// 	return nil
	// }

	// return nil
}

func (c *cache) setCache(ctx context.Context, key string, v []byte, expireSecond int) error {
	for _, c := range c.stores {
		log.Printf("cache data to: %s cache key:[%s]", c.Name(), key)
		if err := c.Put(ctx, key, v, expireSecond); err != nil {
			return err
		}
	}

	return nil
}

func (c *cache) startWatcher() {
	if c.pubsub != nil {
		go func() {
			c.pubsub.Subscribe("")
		}()
	}
}

func (c *cache) isEmpty(data []byte) bool {
	return bytes.Equal(data, c.emptyObjectToken)
}

func acquireDefaultCache() *cache {
	return &cache{
		emptyObjectToken: []byte(defaultRedisEmpty),
		serializer:       serializer{},
		lock:             locker.NewChanSourceLocker(),
		qps:              &qps{},
		maxRetry:         defaultMaxRetry,
	}
}
