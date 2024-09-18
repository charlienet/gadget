package redis

import "context"

type redis_stack_store struct {
	options
	capacity uint
	fpp      float64
}

func (r *redis_stack_store) Initialize(ctx context.Context, keys []uint64, capacity uint, fpp float64) []uint64 {
	r.capacity = capacity
	r.fpp = fpp

	r.rebuild(ctx)

	return r.options.getSetKeys(ctx, keys)
}

func (r redis_stack_store) Add(ctx context.Context, element string, offsets []uint64) {
	r.rdb.BFAdd(ctx, r.key, element)
}

func (r redis_stack_store) Test(ctx context.Context, element string, offsets []uint64) bool {
	exist, _ := r.rdb.BFExists(ctx, r.key, element).Result()
	return exist
}

func (r redis_stack_store) Clear(ctx context.Context) {
	r.rebuild(ctx)
}

func (r redis_stack_store) rebuild(ctx context.Context) {
	r.rdb.Del(ctx, r.key)
	r.rdb.BFReserve(ctx, r.key, r.fpp, int64(r.capacity))
}
