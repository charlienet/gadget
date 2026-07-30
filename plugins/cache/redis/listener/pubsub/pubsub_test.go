package pubsub

import (
	"context"
	"math"
	"testing"
	"time"

	"git.charlienet.top/go/gadget/cache"
	"git.charlienet.top/go/gadget/redis"
	"git.charlienet.top/go/gadget/test"
	"github.com/charlienet/go-misc/random"
)

func TestSS(t *testing.T) {
	println((math.Ln2 * math.Ln2))

	test.RunOnRedisStack(t, func(rdb redis.Client) {
		c := "abc"
		c2 := "abc:dddd"
		r := NewListener(rdb, c)
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
		lis := NewListener(rdb, channel)
		// defer lis.Close()

		lis.Publish("ccc")

		c := cache.New(cache.WithListener(lis))
		defer c.Close()

		key := "abc"

		c.Delete(context.Background(), key)
		time.Sleep(time.Second)
	})
}

func TestListenerReconnection(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		lis := NewListener(rdb, "test-reconnect-channel")
		defer lis.Close()

		err := lis.Publish("test-key")
		if err != nil {
			t.Fatalf("publish failed: %v", err)
		}

		time.Sleep(500 * time.Millisecond)
	})
}
