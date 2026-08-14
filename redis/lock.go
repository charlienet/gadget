package redis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// LockOption 配置分布式锁。
type LockOption func(*lockConfig)

type lockConfig struct {
	ttl           time.Duration // 锁过期时间
	retryInterval time.Duration // Lock 阻塞获取时的重试间隔
	timeout       time.Duration // Lock 阻塞获取的整体超时（0 = 无限）
	token         string        // 锁持有者标识（默认随机生成）
}

const (
	defaultLockTTL          = 30 * time.Second
	defaultLockRetryInterval = 100 * time.Millisecond
)

func defaultLockConfig() lockConfig {
	return lockConfig{
		ttl:           defaultLockTTL,
		retryInterval: defaultLockRetryInterval,
	}
}

// WithTTL 设置锁的过期时间（默认 30 秒）。
func WithTTL(d time.Duration) LockOption {
	return func(c *lockConfig) {
		if d > 0 {
			c.ttl = d
		}
	}
}

// WithRetryInterval 设置 Lock 阻塞获取失败后的重试间隔（默认 100ms）。
func WithRetryInterval(d time.Duration) LockOption {
	return func(c *lockConfig) {
		if d > 0 {
			c.retryInterval = d
		}
	}
}

// WithTimeout 设置 Lock 阻塞获取的整体超时（0 表示无限等待，默认 0）。
// 超时由 Lock 内部派生 ctx 实现，也可直接通过调用方 ctx 取消。
func WithTimeout(d time.Duration) LockOption {
	return func(c *lockConfig) {
		if d >= 0 {
			c.timeout = d
		}
	}
}

// WithToken 指定锁的 token（默认随机生成）。外部指定便于检测重入：
// 同一 token 再次 TryLock 会失败（锁已存在），可结合 Renew 实现看门狗续期。
func WithToken(token string) LockOption {
	return func(c *lockConfig) {
		if token != "" {
			c.token = token
		}
	}
}

// Lock 是基于 Redis SET NX EX 的分布式锁。
// key 的值为随机 token，释放/续期时通过 Lua 校验 token，防止误删他人持有的锁。
//
// 使用示例：
//
//	lock := rdb.NewLock("job:1", redis.WithTTL(10*time.Second))
//	if ok, err := lock.TryLock(ctx); err != nil || !ok {
//		return err // 获取失败：锁被他人持有
//	}
//	defer lock.Unlock(ctx)
//	// ... 执行临界区
type Lock struct {
	client  *redisClient
	key     string
	token   string
	ttl     time.Duration
	retry   time.Duration
	timeout time.Duration
}

// NewLock 创建分布式锁（挂 *redisClient）。
func (rdb *redisClient) NewLock(key string, opts ...LockOption) *Lock {
	cfg := defaultLockConfig()
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.token == "" {
		cfg.token = randomToken()
	}
	return &Lock{
		client:  rdb,
		key:     key,
		token:   cfg.token,
		ttl:     cfg.ttl,
		retry:   cfg.retryInterval,
		timeout: cfg.timeout,
	}
}

// Token 返回本锁实例的 token（外部指定或随机生成）。
func (l *Lock) Token() string {
	return l.token
}

// randomToken 生成随机十六进制 token；随机源失败（极端情况）时退回纳秒时间戳。
func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// TryLock 非阻塞尝试获取锁：SET NX EX（key 不存在时设置并带过期时间）。
// 返回 true 表示获取成功；false 表示锁已被他人持有。
func (l *Lock) TryLock(ctx context.Context) (bool, error) {
	ok, err := l.client.SetNX(ctx, l.key, l.token, l.ttl).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

// unlockScript 原子释放锁：仅当 key 当前值等于本锁 token 时才 DEL，
// 防止误删他人（如锁过期后被他人获取）持有的锁。
var unlockScript = goredis.NewScript(`
	if redis.call('GET', KEYS[1]) == ARGV[1] then
		return redis.call('DEL', KEYS[1])
	end
	return 0
`)

// Unlock 释放锁：仅当锁仍由本实例持有（token 匹配）时删除。
// 不匹配时静默返回 nil（锁已丢失/已被释放，无副作用）。
func (l *Lock) Unlock(ctx context.Context) error {
	_, err := unlockScript.Run(ctx, l.client, []string{l.key}, l.token).Int()
	return err
}

// Lock 阻塞获取锁，直到成功、ctx 取消或 WithTimeout 超时。
// 失败后按 WithRetryInterval 间隔重试。
func (l *Lock) Lock(ctx context.Context) error {
	if l.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.timeout)
		defer cancel()
	}

	for {
		ok, err := l.TryLock(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(l.retry):
		}
	}
}

// renewScript 原子续期：仅当 key 值等于本锁 token 时重新设置过期时间（毫秒）。
var renewScript = goredis.NewScript(`
	if redis.call('GET', KEYS[1]) == ARGV[1] then
		return redis.call('PEXPIRE', KEYS[1], ARGV[2])
	end
	return 0
`)

// Renew 续期锁（看门狗用）：仅当锁仍由本实例持有（token 匹配）时延长 ttl。
// 返回 true 表示续期成功；false 表示锁已丢失（被释放或过期后被他人获取）。
// ttl 必须为正数：传 0 或负值会导致 PEXPIRE 立即过期释放锁，引发临界区并发，
// 此处直接返回参数错误（中文消息）。
func (l *Lock) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("redis: 续期时长必须为正数，got %v", ttl)
	}

	n, err := renewScript.Run(ctx, l.client, []string{l.key}, l.token, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
