package test

import (
	"log"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/charlienet/gadget/redis"
	"github.com/stretchr/testify/assert"
)

type redisOption struct {
	addr     string
	password string
	prefix   string
}

var (
	redisOptions = map[string]redisOption{
		"redis":       {addr: "redis:6379"},
		"redis_stack": {addr: "redis_stack:6380"},
	}
)

func RunOnRedisStack(t assert.TestingT, fn func(rdb redis.Client), opts ...redis.Option) {
	runOnRedis(t, fn, redisOptions["redis_stack"], opts...)
}

func RunOnRedis(t assert.TestingT, fn func(rdb redis.Client), opts ...redis.Option) {
	runOnRedis(t, fn, redisOptions["redis"], opts...)
}

func RunOnMiniRedis(t assert.TestingT, fn func(rdb redis.Client)) {
	run(t, fn, func() (r redis.Client, clean func(), err error) {
		return createMiniRedis()
	})
}

func runOnRedis(t assert.TestingT, fn func(rdb redis.Client), opt redisOption, opts ...redis.Option) {
	run(t, fn, func() (r redis.Client, clean func(), err error) {
		o := make([]redis.Option, 0, len(opts)+3)
		if len(opts) > 0 {
			o = opts
		} else {
			o = append(o, redis.WithAddr(opt.addr))
			o = append(o, redis.WithPassword(opt.password))
			o = append(o, redis.WithPrefix(opt.prefix))
		}

		rdb := redis.New(o...)
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
