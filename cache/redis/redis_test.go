package redis_test

import (
	"context"
	"testing"

	s "github.com/charlienet/gadget/cache/redis"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
)

func TestRedisStore(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		c := s.New(rdb)

		_ = c

		t.Log(c.Get(context.TODO(), "abc"))

		v := []byte("abc")
		c.Set(context.Background(), "abc", v, 20)

		ret, exist, err := c.Get(context.Background(), "abc")
		t.Log(string(ret), exist, err)
	})
}
