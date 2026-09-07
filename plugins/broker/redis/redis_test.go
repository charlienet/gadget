package redis_test

import (
	"testing"

	b "github.com/charlienet/gadget/plugins/broker/redis"
	"github.com/charlienet/gadget/redis"
	mini "github.com/charlienet/gadget/redis/test/mini"
)

func TestRedisBroker(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		broker := b.New(rdb)
		_ = broker
	})
}
