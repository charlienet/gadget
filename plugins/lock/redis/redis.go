// Package redis 提供 lock.Backend 的 Redis 实现。
//
// 获取锁使用 SET key token NX EX ttl（原子占位并设置过期，锁到期自动释放）；
// 释放与续期使用 Lua 脚本，保证「先校验 key 当前值等于本 token、再执行删除 /
// PEXPIRE」的原子性，token 不匹配时不误删、不误续他人锁。Redis 服务不可用时
// 将错误包装为 lock.ErrBackendUnavailable，交由上层 lock 核心按 FailPolicy 兜底。
//
// 由于包名 redis 与 go-redis 冲突，import 时建议起别名 redislock：
//
//	import (
//		goredis "github.com/redis/go-redis/v9"
//		redislock "github.com/charlienet/gadget/plugins/lock/redis"
//	)
//
//	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379"})
//	l := lock.New("order:123", lock.WithBackend(redislock.New(rdb)))
//
//	ok, err := l.TryLock(ctx)
//	if err == nil && ok {
//		defer l.Unlock(ctx)
//		// ... 进入临界区 ...
//	}
package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/charlienet/gadget/lock"
	"github.com/charlienet/gadget/redis"
)

var _ lock.Backend = &Backend{}
var _ lock.Renewer = &Backend{}

// Backend 是 Redis 锁后端，实现 lock.Backend 和 lock.Renewer 接口。
// 使用 SETNX + Lua 脚本保证原子性，token 校验防止误删。
type Backend struct {
	rdb goredis.Cmdable
}

// TryAcquire 非阻塞尝试获取锁：SET NX EX（key 不存在时设置并带过期时间）。
// 返回 true 表示获取成功；false 表示锁已被他人持有。
// Redis 服务不可用时返回 lock.ErrBackendUnavailable 哨兵错误。
func (b *Backend) TryAcquire(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	ok, err := b.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil && redis.IsUnavailable(err) {
		return false, fmt.Errorf("%w: %v", lock.ErrBackendUnavailable, err)
	}
	return ok, err
}

// unlockScript 原子释放锁：仅当 key 当前值等于本锁 token 时才 DEL，
// 防止误删他人（如锁过期后被他人获取）持有的锁。
var unlockScript = goredis.NewScript(`
	if redis.call('GET', KEYS[1]) == ARGV[1] then
		return redis.call('DEL', KEYS[1])
	end
	return 0
`)

// Release 释放锁：仅当锁仍由持有者持有（token 匹配）时删除。
// 不匹配时静默返回 nil（锁已丢失/已被释放）。
func (b *Backend) Release(ctx context.Context, key, token string) error {
	_, err := unlockScript.Run(ctx, b.rdb, []string{key}, token).Int()
	if err != nil && redis.IsUnavailable(err) {
		return fmt.Errorf("%w: %v", lock.ErrBackendUnavailable, err)
	}
	return err
}

// renewScript 原子续期：仅当 key 值等于本锁 token 时重新设置过期时间（毫秒）。
var renewScript = goredis.NewScript(`
	if redis.call('GET', KEYS[1]) == ARGV[1] then
		return redis.call('PEXPIRE', KEYS[1], ARGV[2])
	end
	return 0
`)

// Renew 续期锁：仅当锁仍由持有者持有（token 匹配）时延长 ttl。
// 返回 true 表示续期成功；false 表示锁已丢失。
func (b *Backend) Renew(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	n, err := renewScript.Run(ctx, b.rdb, []string{key}, token, ttl.Milliseconds()).Int()
	if err != nil {
		if redis.IsUnavailable(err) {
			return false, fmt.Errorf("%w: %v", lock.ErrBackendUnavailable, err)
		}
		return false, err
	}
	return n == 1, nil
}
