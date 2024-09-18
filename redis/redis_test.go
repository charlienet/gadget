package redis_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
	"github.com/charlienet/go-misc/random"
	"github.com/go-redis/redis_rate/v10"
	"github.com/stretchr/testify/assert"
)

func TestNewRedis(t *testing.T) {
	rdb := redis.New(
		redis.WithAddrs([]string{"192.168.2.222:6379"}),
		redis.WithPassword("123456"))

	assert.Nil(t, rdb.Constraint(redis.Ping()))
}

func TestRunMiniRedis(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		rdb.Constraint(redis.Ping())
	})
}

func TestVersion(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		assert.NotNil(t, rdb.Constraint(redis.Version(">=10.0")))
	})
}

func TestPrefix(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		r1 := rdb.AddPrefix("h2")
		r1.Set(context.Background(), "abc", "abc", time.Hour)
	})
}

func TestIsStack(t *testing.T) {

	t.Run("is mini redis", func(t *testing.T) {
		test.RunOnMiniRedis(t, func(rdb redis.Client) {
			t.Log(rdb.IsStack())
		})
	})

	t.Run("is no stack", func(t *testing.T) {
		test.RunOnRedis(t, func(rdb redis.Client) {
			t.Log(rdb.IsStack())
		})
	})

	t.Run("is stack", func(t *testing.T) {
		test.RunOnRedis(t, func(rdb redis.Client) {
			t.Log(rdb.IsStack())
		}, redis.WithAddr("192.168.3.200:6380"))
	})
}

func TestBf(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		key := "ffff"
		rdb.Del(context.Background(), key)

		rdb.CFReserve(context.Background(), "ccc", 1000000)

		if err := rdb.BFReserve(context.Background(), "ffff", 0.01, 1000000).Err(); err != nil {
			t.Fatal(err)
		}

		for i := 0; i < 10000; i++ {
			rdb.BFAdd(context.Background(), "ffff", i)
		}
	})
}

func BenchmarkBF(b *testing.B) {
	key := "abcdef"

	test.RunOnRedisStack(b, func(rdb redis.Client) {
		rdb.BFReserve(context.Background(), key, 0.0001, 100000)
		ctx := context.Background()

		b.Run("bf", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				rdb.BFExists(ctx, key, random.Hex.Generate(1))
			}
		})
	})

}

func TestRateLimiter(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		if err := rdb.FlushDB(context.Background()).Err(); err != nil {
			panic(err)
		}

		limiter := redis_rate.NewLimiter(rdb)
		for i := 0; i < 3; i++ {
			res, err := limiter.Allow(context.Background(), "project:123", redis_rate.PerSecond(10))
			if err != nil {
				panic(err)
			}

			fmt.Println("allowed", res.Allowed, "remaining", res.Remaining)
		}

	})
}
