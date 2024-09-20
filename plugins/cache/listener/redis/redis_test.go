package redis

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
	"github.com/charlienet/go-misc/random"
)

func TestSS(t *testing.T) {
	println((math.Ln2 * math.Ln2))

	test.RunOnRedis(t, func(rdb redis.Client) {
		c := "abc"
		c2 := "abc:dddd"
		r := newReidsStore(rdb, c)
		defer r.Close()

		time.Sleep(time.Second)
		for i := 0; i < 10; i++ {
			r.Publish(random.Hex.Generate(12))
		}

		for i := 'A'; i < 'Z'; i++ {
			rdb.Publish(context.TODO(), c2, i)
		}

		time.Sleep(time.Second * 3)
	})
}
