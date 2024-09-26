package bigcache

import (
	"context"
	"errors"
	"time"

	"github.com/allegro/bigcache/v3"
)

type bigcache_store struct {
	cache *bigcache.BigCache
}

func NewBigCache() *bigcache_store {
	c, _ := bigcache.New(context.Background(), bigcache.DefaultConfig(time.Minute))
	return &bigcache_store{cache: c}
}

func (f *bigcache_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	data, err := f.cache.Get(key)
	if err != nil {
		if errors.Is(err, bigcache.ErrEntryNotFound) {
			return data, false, nil
		}
		return data, false, err
	}

	return data, true, nil
}

func (f *bigcache_store) Put(ctx context.Context, key string, v []byte, expirSecond int) error {
	f.cache.Stats()
	return f.cache.Set(key, v)
}

func (f *bigcache_store) Delete(ctx context.Context, key ...string) error {
	for _, k := range key {
		if err := f.cache.Delete(k); err != nil {
			return err
		}
	}

	return nil
}

func (f *bigcache_store) Clear() {
	f.cache.Reset()
}

func (*bigcache_store) Name() string { return "bigcache" }

func (*bigcache_store) IsRemote() bool { return false }
