package redis

import (
	"context"
	"errors"
	"time"

	"github.com/charlienet/gadget/redis"
)

type redis_store struct {
	rdb redis.Client
}

func New(rdb redis.Client) *redis_store {
	return &redis_store{
		rdb: rdb,
	}
}

func (f *redis_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	str, err := f.rdb.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.NotFound) {
			return []byte{}, false, nil
		}

		return []byte{}, false, err
	}

	return []byte(str), true, nil
}

func (r *redis_store) Set(ctx context.Context, key string, v []byte, expireSeconds int) error {
	expire := time.Second * time.Duration(expireSeconds)
	return r.rdb.Set(ctx, key, v, expire).Err()
}

func (r *redis_store) Delete(ctx context.Context, key ...string) error {
	return r.rdb.Del(ctx, key...).Err()
}

func (*redis_store) Name() string { return "Redis" }
