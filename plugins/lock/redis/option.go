package redis

import (
	goredis "github.com/redis/go-redis/v9"

	"github.com/charlienet/gadget/lock"
)

// Option 自定义 Backend 的构造选项（预留扩展）。
type Option func(*Backend)

// New 创建 Redis 锁后端。rdb 必传，nil 时 panic。
// 返回的 Backend 同时实现 lock.Backend 和 lock.Renewer 接口。
//
// rdb 可传入任何满足 goredis.Cmdable 的值（*redis.Client、
// *redis.ClusterClient、或 github.com/charlienet/gadget/redis.Client）。
func New(rdb goredis.Cmdable, opts ...Option) lock.Backend {
	if rdb == nil {
		panic("redislock: nil redis client")
	}
	b := &Backend{rdb: rdb}
	for _, o := range opts {
		o(b)
	}
	return b
}
