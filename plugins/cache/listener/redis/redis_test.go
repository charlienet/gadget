package redis

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
	"github.com/charlienet/go-misc/random"
)

func TestSS(t *testing.T) {
	println((math.Ln2 * math.Ln2))

	test.RunOnRedisStack(t, func(rdb redis.Client) {
		c := "abc"
		c2 := "abc:dddd"
		r := NewReidsListener(rdb, c)
		defer r.Close()

		count := 0
		go func() {
			c := r.Subscribe()
			for key := range c {
				count++
				t.Log("delete:", key)
			}
		}()

		time.Sleep(time.Second)
		for range 10 {
			r.Publish(random.Hex.Generate(12))
		}

		for i := 'A'; i < 'Z'; i++ {
			rdb.Publish(context.TODO(), c2, i)
		}

		time.Sleep(time.Second * 3)
	})
}

func TestCacheWatch(t *testing.T) {
	channel := "abcdef"
	test.RunOnRedis(t, func(rdb redis.Client) {
		lis := NewReidsListener(rdb, channel)
		// defer lis.Close()

		lis.Publish("ccc")

		c := cache.New(cache.WithListener(lis))
		defer c.Close()

		key := "abc"

		c.Delete(context.Background(), key)
		time.Sleep(time.Second)
	})
}
