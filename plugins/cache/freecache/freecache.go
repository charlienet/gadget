package freecache

import (
	"context"
	"errors"

	"github.com/coocood/freecache"
)

type freecache_store struct {
	cache *freecache.Cache
}

func new(size int) *freecache_store {
	c := freecache.NewCache(size)

	return &freecache_store{
		cache: c,
	}
}

func (f *freecache_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, err := f.cache.Get([]byte(key))
	if err != nil {
		if errors.Is(err, freecache.ErrNotFound) {
			return []byte{}, false, nil
		} else {
			return []byte{}, false, err
		}
	}

	return value, true, nil
}

func (f *freecache_store) Put(ctx context.Context, key string, v []byte, expireSeconds int) error {
	return f.cache.Set([]byte(key), v, expireSeconds)
}

func (f *freecache_store) Delete(ctx context.Context, key ...string) error {
	for _, k := range key {
		affected := f.cache.Del([]byte(k))
		_ = affected
	}

	return nil
}

func (f *freecache_store) Clear() {
	f.cache.Clear()
}

func (r *freecache_store) Name() string { return "freecache" }

func (*freecache_store) IsRemote() bool { return false }
