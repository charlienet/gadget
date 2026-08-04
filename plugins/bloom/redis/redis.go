package redis

import (
	"context"
	_ "embed"
	"sync"

	goredis "github.com/redis/go-redis/v9"

	"github.com/charlienet/gadget/bloom"
	"github.com/charlienet/gadget/redis"
)

type redis_store struct {
	options
	lock sync.RWMutex
}

func New(rdb redis.Client, key string) bloom.Store {
	store := createRedisStore(options{
		rdb: rdb,
		key: key,
	})

	return store
}

func (r *redis_store) Initialize(ctx context.Context, keys []uint64, capacity uint, fpp float64) []uint64 {
	return r.options.getSetKeys(ctx, keys)
}

func (r *redis_store) Add(ctx context.Context, element string, offsets []uint64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	pipe := r.rdb.Pipeline()
	for _, p := range offsets {
		pipe.SetBit(ctx, r.key, int64(p), 1)
	}

	pipe.Exec(ctx)
}

func (r *redis_store) Test(ctx context.Context, element string, offsets []uint64) bool {
	for _, p := range offsets {
		i, _ := r.rdb.GetBit(ctx, r.key, int64(p)).Result()
		if i == 1 {
			return true
		}
	}

	return false
}

func (r *redis_store) Clear(ctx context.Context) {
	r.rdb.Del(ctx, r.key)
}

func (r *redis_store) AddMulti(ctx context.Context, elements []string, offsets [][]uint64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	pipe := r.rdb.Pipeline()
	for _, offsetsForElement := range offsets {
		for _, p := range offsetsForElement {
			pipe.SetBit(ctx, r.key, int64(p), 1)
		}
	}

	pipe.Exec(ctx)
}

func (r *redis_store) TestMulti(ctx context.Context, elements []string, offsets [][]uint64) []bool {
	results := make([]bool, len(elements))
	
	// Use pipeline to batch GetBit operations
	pipe := r.rdb.Pipeline()
	cmds := make([][]*goredis.IntCmd, len(elements))
	
	for i, offsetsForElement := range offsets {
		cmds[i] = make([]*goredis.IntCmd, len(offsetsForElement))
		for j, p := range offsetsForElement {
			cmds[i][j] = pipe.GetBit(ctx, r.key, int64(p))
		}
	}
	
	pipe.Exec(ctx)
	
	// Check results: element exists if any bit is 1
	for i, cmdList := range cmds {
		for _, cmd := range cmdList {
			if cmd.Val() == 1 {
				results[i] = true
				break
			}
		}
	}
	
	return results
}

func createRedisStore(opt options) bloom.Store {
	// redis stack
	if opt.rdb.IsStack() {
		return &redis_stack_store{options: opt}
	}

	// redis versions greater than 7.0
	if err := opt.rdb.Constraint(redis.Version(">=7.0")); err == nil {
		return &reids_high_version_store{options: opt}
	}

	return &redis_store{options: opt}
}
