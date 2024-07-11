package tinylfu

import (
	"context"
	"sync"
	"time"

	"github.com/vmihailenco/go-tinylfu"
)

type TinyLFU struct {
	sync.RWMutex
	lfu *tinylfu.T
	ttl time.Duration
}

type tinyStoreItem struct {
	Value []byte
}

func NewTinyLFU(size int, ttl time.Duration) *TinyLFU {
	return &TinyLFU{
		lfu: tinylfu.New(size, 1000),
		ttl: ttl,
	}
}

func (f *TinyLFU) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, exist := f.lfu.Get(key)
	if !exist {
		return []byte{}, false, nil
	}

	if item, ok := value.(tinyStoreItem); ok {
		return item.Value, true, nil
	}

	return []byte{}, false, nil
}

func (f *TinyLFU) Set(ctx context.Context, key string, v []byte, expirSecond int) error {
	expireAt := time.Time{}
	if expirSecond > 0 {
		expireAt = time.Now().Add(time.Second * time.Duration(expirSecond))
	}

	f.lfu.Set(&tinylfu.Item{
		Key: key,
		Value: tinyStoreItem{
			Value: v,
		},
		ExpireAt: expireAt,
	})

	return nil
}

func (f *TinyLFU) Delete(ctx context.Context, key ...string) error {
	for _, k := range key {
		f.lfu.Del(k)
	}
	return nil
}

func (f *TinyLFU) Clear() {
}

func (*TinyLFU) Name() string { return "TinyLfu" }
