package redis_test

import (
	"testing"

	b "github.com/charlienet/gadget/plugins/broker/redis"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/redis/test"
)

func TestRedisBroker(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		broker := b.New(rdb)
		_ = broker
	})
}
