package cache

import (
	"time"

	"github.com/charlienet/gadget/cache/bigcache"
	"github.com/charlienet/gadget/cache/freecache"
	r "github.com/charlienet/gadget/cache/redis"
	"github.com/charlienet/gadget/cache/tinylfu"
	"github.com/charlienet/gadget/redis"
)

type option func(*cache)

func WithRedis(rdb redis.Client) option {
	return func(c *cache) {
		c.distributed = r.New(rdb)
	}
}

func WithRedisPubSub(rdb redis.Client) option {
	return func(c *cache) {
	}
}

func WithTinyLFU(size int, ttl time.Duration) option {
	return func(c *cache) {
		c.local = tinylfu.NewTinyLFU(size, ttl)
	}
}

func WithFreecache() option {
	return func(c *cache) {
		c.local = freecache.New()
	}
}

func WithBigcache() option {
	return func(c *cache) {
		c.local = bigcache.NewBigCache()
	}
}

func WithEmptyToken(empty string) option {
	return func(c *cache) {
		c.emptyObjectToken = []byte(empty)
	}
}

func WithMaxRetry(retry int) option {
	return func(c *cache) {
		if retry <= 1 {
			c.maxRetry = 1
		} else {
			c.maxRetry = retry
		}
	}
}

func Disable() option {
	return func(c *cache) {
		c.disable = true
	}
}
