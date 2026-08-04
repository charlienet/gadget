package test

import (
	"log"
	"os"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/charlienet/gadget/redis"
	"github.com/stretchr/testify/assert"
)

func RunOnRedisStack(t assert.TestingT, fn func(rdb redis.Client), opts ...redis.Option) {
	url := os.Getenv("REDIS_STACK_URL")
	if len(url) == 0 {
		assert.Fail(t, "REDIS_URL not defined")
	}

	runOnRedis(t, fn, url, opts...)
}

func RunOnRedis(t assert.TestingT, fn func(rdb redis.Client), opts ...redis.Option) {
	url := os.Getenv("REDIS_URL")
	assert.NotEmpty(t, url)

	runOnRedis(t, fn, url, opts...)
}

func RunOnMiniRedis(t assert.TestingT, fn func(rdb redis.Client)) {
	run(t, fn, func() (r redis.Client, clean func(), err error) {
		return createMiniRedis()
	})
}

func runOnRedis(t assert.TestingT, fn func(rdb redis.Client), url string, opts ...redis.Option) {
	run(t, fn, func() (r redis.Client, clean func(), err error) {
		rdb, err := redis.NewWithUrl(url, opts...)
		assert.Nil(t, err)

		if err := rdb.Constraint(redis.Ping()); err != nil {
			return nil, nil, err
		}

		return rdb, func() { rdb.Close() }, nil
	})
}

func run(t assert.TestingT, fn func(rdb redis.Client), cn func() (r redis.Client, clean func(), err error)) {
	r, clean, err := cn()

	assert.Nil(t, err, err)
	defer clean()
	fn(r)
}

func createMiniRedis() (r redis.Client, clean func(), err error) {
	mr, err := miniredis.Run()
	if err != nil {
		return nil, nil, err
	}

	addr := mr.Addr()
	log.Println("mini redis run at:", addr)

	rdb := redis.New(redis.WithAddr(addr))

	return rdb, func() {
		ch := make(chan struct{})

		go func() {
			rdb.Close()
			mr.Close()
			close(ch)
		}()

		select {
		case <-ch:
		case <-time.After(time.Second * 5):
		}
	}, nil
}
