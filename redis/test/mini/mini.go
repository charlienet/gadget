// Package mini 提供基于内存 Redis（miniredis）的测试入口，是 gadget/redis
// 测试辅助层中唯一会拉入 miniredis 依赖的包。
//
// redis/test 主包刻意不含 mini 能力：下游应用 import redis/test 不会因它
// 在 go.mod 引入 miniredis（Go 1.17+ 模块图剪枝按被 import 包的传递依赖
// 闭包传导）。需要内存 Redis 跑测试时，显式 import 本包并调用 mini.Run。
package mini

import (
	"log"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/charlienet/gadget/redis"
	"github.com/stretchr/testify/assert"
)

// Run 在内存版 miniredis 上运行 fn（无需任何环境变量）。
func Run(t testing.TB, fn func(rdb redis.Client)) {
	r, clean, err := createMiniRedis()

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
			_ = rdb.Close()
			mr.Close()
			close(ch)
		}()

		select {
		case <-ch:
		case <-time.After(time.Second * 5):
		}
	}, nil
}
