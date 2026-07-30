package redis_test

import (
	"testing"

	b "git.charlienet.top/go/gadget/plugins/broker/redis"
	"git.charlienet.top/go/gadget/redis"
	"git.charlienet.top/go/gadget/test"
)

func TestRedisBroker(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		broker := b.New(rdb)
		_ = broker
	})
}
