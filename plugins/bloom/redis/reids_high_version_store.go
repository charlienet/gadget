package redis

import (
	"context"
	_ "embed"
)

//go:embed bloom.lua
var redis_bloom_function string

type reids_high_version_store struct {
	options
}

func (r *reids_high_version_store) Initialize(ctx context.Context, keys []uint64, capacity uint, fpp float64) []uint64 {
	r.rdb.FunctionLoad(ctx, redis_bloom_function)

	return r.options.getSetKeys(ctx, keys)
}

func (r reids_high_version_store) Add(ctx context.Context, element string, offsets []uint64) {
	r.rdb.FCall(ctx, "set_bit", []string{r.key}, r.buildOffsetArgs(offsets)...)
}

func (r reids_high_version_store) Test(ctx context.Context, element string, offsets []uint64) bool {
	resp, _ := r.rdb.FCall(ctx, "test_bit", []string{r.key}, r.buildOffsetArgs(offsets)...).Result()
	exists, ok := resp.(int64)
	if !ok {
		return false
	}

	return exists == 1
}

func (r reids_high_version_store) Clear(ctx context.Context) {
	r.rdb.Del(ctx, r.key)
}

func (r reids_high_version_store) buildOffsetArgs(offsets []uint64) []any {
	args := make([]any, len(offsets))
	for i, p := range offsets {
		args[i] = p
	}

	return args
}
