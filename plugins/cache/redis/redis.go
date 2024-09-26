package redis

import (
	"context"
	"errors"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/go-misc/random"
)

const (
	defaultRedisTTLFactor = 30
)

type redis_store struct {
	rdb       redis.Client
	ttlFactor int
}

func new(rdb redis.Client, opts ...option) cache.Store {
	s := &redis_store{rdb: rdb, ttlFactor: defaultRedisTTLFactor}
	for _, o := range opts {
		o(s)
	}

	return s
}

func (f *redis_store) Initialize(opt cache.Options) {
	if len(opt.Name) > 0 {
		f.rdb = f.rdb.AddPrefix(opt.Name)
	}
}

func (f *redis_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	data, err := f.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.NotFound) {
			return []byte{}, false, nil
		}

		return []byte{}, false, err
	}

	return data, true, nil
}

func (r *redis_store) Put(ctx context.Context, key string, data []byte, expireSeconds int) error {
	// 超时时间添加随机秒数
	factor := 0
	if r.ttlFactor > 1 {
		factor = random.IntRange(1, r.ttlFactor)
	}

	expire := time.Second * time.Duration(expireSeconds+factor)
	return r.rdb.Set(ctx, key, data, expire).Err()
}

func (r *redis_store) Delete(ctx context.Context, key ...string) error {
	return r.rdb.Del(ctx, key...).Err()
}

func (*redis_store) Name() string { return "Redis" }

func (*redis_store) IsRemote() bool { return true }
