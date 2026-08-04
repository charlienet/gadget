package redis

import (
	"context"
	_ "embed"

	goredis "github.com/redis/go-redis/v9"
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

func (r reids_high_version_store) AddMulti(ctx context.Context, elements []string, offsets [][]uint64) {
	pipe := r.rdb.Pipeline()
	for i := range elements {
		if i < len(offsets) {
			pipe.FCall(ctx, "set_bit", []string{r.key}, r.buildOffsetArgs(offsets[i])...)
		}
	}
	pipe.Exec(ctx)
}

func (r reids_high_version_store) TestMulti(ctx context.Context, elements []string, offsets [][]uint64) []bool {
	results := make([]bool, len(elements))
	pipe := r.rdb.Pipeline()
	cmds := make([]*goredis.Cmd, len(elements))
	
	for i := range elements {
		if i < len(offsets) {
			cmds[i] = pipe.FCall(ctx, "test_bit", []string{r.key}, r.buildOffsetArgs(offsets[i])...)
		}
	}
	
	pipe.Exec(ctx)
	
	for i, cmd := range cmds {
		if cmd == nil {
			continue
		}
		resp, _ := cmd.Result()
		if exists, ok := resp.(int64); ok && exists == 1 {
			results[i] = true
		}
	}
	
	return results
}
