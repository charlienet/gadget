package test

import (
	"log"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/charlienet/gadget/redis"
	"github.com/stretchr/testify/assert"
)

const (
	addr     = "redis:6379"
	password = "123456"
	prefix   = "prefix"
)

func RunOnRedis(t assert.TestingT, fn func(rdb redis.Client), opts ...redis.Option) {
	run(t, fn, func() (r redis.Client, clean func(), err error) {
		o := make([]redis.Option, 0, len(opts)+3)
		if len(opts) > 0 {
			o = opts
		} else {
			o = append(o, redis.WithAddr(addr))
			o = append(o, redis.WithPassword(password))
			o = append(o, redis.WithPrefix(prefix))
		}

		rdb := redis.New(o...)
		if err := rdb.Constraint(redis.Ping()); err != nil {
			return nil, nil, err
		}

		return rdb, func() { rdb.Close() }, nil
	})
}

func RunOnMiniRedis(t assert.TestingT, fn func(rdb redis.Client)) {
	run(t, fn, func() (r redis.Client, clean func(), err error) {
		return createMiniRedis()
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
