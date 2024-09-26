package gcache

import (
	"context"
	"errors"
	"time"

	"github.com/bluele/gcache"
)

type gcache_store struct {
	s gcache.Cache
}

func newGcache(size int) gcache_store {
	c := gcache.New(size).
		LRU().
		Build()

	return gcache_store{s: c}
}

func (c gcache_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	r, err := c.s.Get(key)

	if err != nil {
		if errors.Is(err, gcache.KeyNotFoundError) {
			return nil, false, nil
		}

		return nil, false, err
	}

	return r.([]byte), true, nil
}

func (c gcache_store) Put(ctx context.Context, key string, v []byte, expireSeconds int) error {
	return c.s.SetWithExpire(key, v, time.Second*time.Duration(expireSeconds))
}

func (c gcache_store) Delete(ctx context.Context, keys ...string) error {
	for _, k := range keys {
		c.s.Remove(k)
	}

	return nil
}

func (c gcache_store) IsRemote() bool { return false }
func (gcache_store) Name() string     { return "gcache" }
