package memcached

import (
	"context"
	"errors"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/charlienet/gadget/cache"
)

var _ cache.Store = &memcached{}

type memcached struct {
	m *memcache.Client
}

func new(addrs ...string) *memcached {
	m := memcache.New(addrs...)
	return &memcached{
		m: m,
	}
}

func (m *memcached) Put(ctx context.Context, key string, v []byte, expireSeconds int) error {
	return m.m.Set(&memcache.Item{
		Key:        key,
		Value:      v,
		Expiration: int32(expireSeconds),
	})
}

func (m *memcached) Get(ctx context.Context, key string) ([]byte, bool, error) {
	item, err := m.m.Get(key)
	if err != nil {
		if errors.Is(err, memcache.ErrCacheMiss) {
			return nil, false, nil
		}
		return nil, false, err
	}

	return item.Value, true, nil
}

func (m *memcached) Delete(ctx context.Context, keys ...string) error {
	for _, key := range keys {
		if err := m.m.Delete(key); err != nil {
			return err
		}
	}

	return nil
}

func (*memcached) Name() string { return "memcache" }

func (*memcached) IsRemote() bool { return true }
