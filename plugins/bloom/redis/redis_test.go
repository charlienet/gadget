package redis_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/charlienet/gadget/bloom"
	r "github.com/charlienet/gadget/plugins/bloom/redis"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
	"github.com/stretchr/testify/assert"
)

func randomHex(n int) string {
	bytes := make([]byte, n/2+1) // Generate enough bytes
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	hexStr := hex.EncodeToString(bytes)
	return hexStr[:n] // Return only n characters
}

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
		bf := bloom.NewOptimal(10000, 0.00001, bloom.WithStore(store))

		ctx := context.Background()

		for i := 0; i < 1000; i++ {
			store.Add(ctx, randomHex(2), []uint64{})
		}

		bf.Exist(ctx, "ABC")
		bf.Exist(ctx, "ABC")

		for i := 0; i < 10000; i++ {
			bf.Exist(ctx, randomHex(2))
		}
	})
}

func TestRedisStoreAddMulti(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		s := r.New(rdb, "test_addmulti")
		ctx := context.Background()

		// 清理
		s.Clear(ctx)

		// 批量添加
		elements := []string{"elem1", "elem2", "elem3"}
		offsets := [][]uint64{
			{1, 2, 3},
			{4, 5, 6},
			{7, 8, 9},
		}

		s.AddMulti(ctx, elements, offsets)

		// 验证每个元素都能被检测到
		for i, elem := range elements {
			assert.True(t, s.Test(ctx, elem, offsets[i]), "element %s should exist", elem)
		}

		// 验证不存在的元素
		assert.False(t, s.Test(ctx, "notexist", []uint64{100, 101, 102}))

		// 清理
		s.Clear(ctx)
	})
}

func TestRedisStackStoreAddMulti(t *testing.T) {
	test.RunOnRedisStack(t, func(rdb redis.Client) {
		s := r.New(rdb, "test_stack_addmulti")
		ctx := context.Background()

		// 清理
		s.Clear(ctx)

		// 批量添加
		elements := []string{"stack_elem1", "stack_elem2", "stack_elem3"}
		offsets := [][]uint64{
			{10, 20, 30},
			{40, 50, 60},
			{70, 80, 90},
		}

		s.AddMulti(ctx, elements, offsets)

		// 验证每个元素都能被检测到
		for i, elem := range elements {
			assert.True(t, s.Test(ctx, elem, offsets[i]), "element %s should exist", elem)
		}

		// 清理
		s.Clear(ctx)
	})
}

func TestBloomFilterAddMulti(t *testing.T) {
	test.RunOnRedisStack(t, func(rdb redis.Client) {
		store := r.New(rdb, "test_bf_addmulti")
		bf := bloom.NewOptimal(10000, 0.001, bloom.WithStore(store))
		ctx := context.Background()

		// 清理
		bf.Clear(ctx)

		// 批量添加
		elements := []string{"bf_elem1", "bf_elem2", "bf_elem3", "bf_elem4", "bf_elem5"}
		bf.AddMulti(ctx, elements...)

		// 验证每个元素都存在
		for _, elem := range elements {
			assert.True(t, bf.Exist(ctx, elem), "element %s should exist", elem)
		}

		// 验证不存在的元素
		assert.False(t, bf.Exist(ctx, "not_exist_elem"))

		// 清理
		bf.Clear(ctx)
	})
}

func BenchmarkRedis(b *testing.B) {
	test.RunOnRedisStack(b, func(rdb redis.Client) {
		store := r.New(rdb, "tessss")
		bf := bloom.NewOptimal(10000, 0.00001, bloom.WithStore(store))
		ctx := context.Background()

		for i := 0; i < 1000; i++ {
			store.Add(ctx, randomHex(2), []uint64{})
		}

		key := "AB"
		bf.Add(ctx, key)

		b.Run("redis stack", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				bf.Exist(ctx, randomHex(2))
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
