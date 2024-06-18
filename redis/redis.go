package redis

import (
	"github.com/redis/go-redis/v9"
)

func New(opts ...Option) {

}

func new(conf *redis.UniversalOptions) {
	rdb := redis.NewUniversalClient(conf)

	_ = rdb
}
