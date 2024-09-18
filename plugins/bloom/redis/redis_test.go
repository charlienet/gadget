package redis_test

import (
	"context"
	"testing"

	"github.com/charlienet/gadget/bloom"
	r "github.com/charlienet/gadget/plugins/bloom/redis"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
	"github.com/charlienet/go-misc/random"
	"github.com/stretchr/testify/assert"
)

func TestRedisMiniStore(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		s := r.New(rdb, "aaaaa")

		ele := "abc"
		off := []uint64{1, 2, 4}
		ctx := context.Background()
		s.Add(ctx, ele, off)

		assert.True(t, s.Test(ctx, ele, off))
		s.Clear(ctx)

		assert.False(t, s.Test(ctx, ele, off))
	})
}

func TestRedisStore(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		s := r.New(rdb, "aaaaa")

		ele := "abc"
		off := []uint64{1, 2, 4}
		ctx := context.Background()
		s.Add(ctx, ele, off)

		assert.True(t, s.Test(ctx, ele, off))
		s.Clear(ctx)

		assert.False(t, s.Test(ctx, ele, off))
	})
}

func TestRedisStackStore(t *testing.T) {
	test.RunOnRedisStack(t, func(rdb redis.Client) {
		s := r.New(rdb, "aaaaa")

		ele := "abc"
		off := []uint64{1, 2, 4}
		ctx := context.Background()
		s.Add(ctx, ele, off)

		assert.True(t, s.Test(ctx, ele, off))
		s.Clear(ctx)

		assert.False(t, s.Test(ctx, ele, off))
	})
}

func TestRedisStack(t *testing.T) {
	test.RunOnRedisStack(t, func(rdb redis.Client) {
		store := r.New(rdb, "tessss")
		bf := bloom.New(10000, 0.00001, bloom.WithStore(store))

		ctx := context.Background()

		for i := 0; i < 1000; i++ {
			store.Add(ctx, random.Hex.Generate(2), []uint64{})
		}

		bf.Exist(ctx, "ABC")
		bf.Exist(ctx, "ABC")

		for i := 0; i < 10000; i++ {
			bf.Exist(ctx, random.Hex.Generate(2))
		}
	})
}

func BenchmarkRedis(b *testing.B) {
	test.RunOnRedisStack(b, func(rdb redis.Client) {
		store := r.New(rdb, "tessss")
		bf := bloom.New(10000, 0.00001, bloom.WithStore(store))
		ctx := context.Background()

		for i := 0; i < 1000; i++ {
			store.Add(ctx, random.Hex.Generate(2), []uint64{})
		}

		key := "AB"
		bf.Add(ctx, key)

		b.Run("redis stack", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				bf.Exist(ctx, random.Hex.Generate(2))
			}
		})
	})

	// test.RunOnRedisStack(b, func(rdb redis.Client) {
	// 	bf := bloom.New(10000, 0.00001, bloom.WithStore(r.New(rdb, "tessss")))

	// 	ctx := context.Background()

	// 	b.RunParallel(func(p *testing.PB) {
	// 		for p.Next() {
	// 			bf.Exist(ctx, random.Hex.Generate(3))
	// 		}
	// 	})
	// })
}
