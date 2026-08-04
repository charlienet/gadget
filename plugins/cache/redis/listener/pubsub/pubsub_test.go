package pubsub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
)

func randomHex(n int) string {
	bytes := make([]byte, n/2+1) // Generate enough bytes
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	hexStr := hex.EncodeToString(bytes)
	return hexStr[:n] // Return only n characters
}

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
			r.Publish(randomHex(12))
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
