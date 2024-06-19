package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
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
