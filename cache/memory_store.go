package cache

import (
	"context"
	"sync"
	"time"
)

type mem_store struct {
	items map[string]item
	sync.RWMutex
}

func NewStore() Store {
	return &mem_store{
		items: make(map[string]item),
	}
}

func (s *mem_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	s.RWMutex.RLock()
	i, found := s.items[key]
	s.RWMutex.RUnlock()

	if !found {
		return nil, false, nil
	}

	if i.Expired() {
		s.RWMutex.Lock()
		delete(s.items, key)
		s.RWMutex.Unlock()

		return nil, false, nil
	}

	return i.Value, true, nil
}

func (s *mem_store) Put(ctx context.Context, key string, v []byte, expireSecond int) error {
	var e int64
	if expireSecond > 0 {
		e = time.Now().Add(time.Second * time.Duration(expireSecond)).UnixNano()
	}

	s.RWMutex.Lock()
	defer s.RWMutex.Unlock()

	s.items[key] = item{
		Value:      v,
		Expiration: e,
	}

	return nil
}

func (s *mem_store) Delete(ctx context.Context, key ...string) error {
	s.RWMutex.Lock()
	defer s.RWMutex.Unlock()

	for _, k := range key {
		delete(s.items, k)
	}

	return nil
}

func (*mem_store) IsRemote() bool { return false }

func (*mem_store) Name() string { return "memory" }

type item struct {
	Value      []byte
	Expiration int64
}

func (i *item) Expired() bool {
	if i.Expiration == 0 {
		return false
	}

	return time.Now().UnixNano() > i.Expiration
}
