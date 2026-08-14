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
	failPolicyConfig
	ttl           time.Duration // 锁过期时间
	retryInterval time.Duration // Lock 阻塞获取时的重试间隔
	timeout       time.Duration // Lock 阻塞获取的整体超时（0 = 无限）
	token         string        // 锁持有者标识（默认随机生成）
}

// LockConfig 是 Lock 的配置类型别名，供 WithFailPolicy 泛型参数使用。
type LockConfig = lockConfig

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
	policy  FailPolicy // 失效兜底策略（默认 FailClosed）
}

// NewLock 创建分布式锁（挂 *redisClient）。
// 失效兜底策略默认 FailClosed（服务不可用时不放行临界区，避免并发写数据）；
// 可用 WithFailPolicy 显式改为 FailOpen（警告：失效放行临界区有并发风险）。
func (rdb *redisClient) NewLock(key string, opts ...LockOption) *Lock {
	cfg := defaultLockConfig()
	cfg.policy = FailClosed // 锁默认 FailClosed：宁可失败也不放行
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
		policy:  cfg.policy,
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

// tryLockRaw 执行 SET NX EX（不做失效兜底，返回原始错误，供 Lock 使用）。
func (l *Lock) tryLockRaw(ctx context.Context) (bool, error) {
	return l.client.SetNX(ctx, l.key, l.token, l.ttl).Result()
}

// TryLock 非阻塞尝试获取锁：SET NX EX（key 不存在时设置并带过期时间）。
// 返回 true 表示获取成功；false 表示锁已被他人持有。
// Redis 服务失效时按兜底策略：FailClosed → (false, nil)（不放行临界区）；
// FailOpen → (true, nil)（放行，显式选择的风险由调用方承担）。
func (l *Lock) TryLock(ctx context.Context) (bool, error) {
	ok, err := l.tryLockRaw(ctx)
	if err != nil {
		if isUnavailable(err) {
			return l.fallbackBool(), fallbackErr(err)
		}
		return false, err
	}
	return ok, nil
}

// fallbackBool 按策略返回兜底值：FailOpen → true（放行）、FailClosed → false。
func (l *Lock) fallbackBool() bool {
	return l.policy == FailOpen
}

// fallbackErr 包装兜底错误：无论 FailOpen/FailClosed 都返回
// ErrRedisUnavailable 哨兵错误（errors.Is 可感知兜底发生）。
func (l *Lock) fallbackErr(err error) error {
	return fallbackErr(err)
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
// Redis 服务失效时按兜底策略：FailClosed → 返回错误；FailOpen → 吞错
// 返回 nil（锁最终靠 TTL 过期释放，不阻塞业务）。
func (l *Lock) Unlock(ctx context.Context) error {
	_, err := unlockScript.Run(ctx, l.client, []string{l.key}, l.token).Int()
	if err != nil && isUnavailable(err) {
		return l.fallbackErr(err)
	}
	return err
}

// Lock 阻塞获取锁，直到成功、ctx 取消或 WithTimeout 超时。
// 失败后按 WithRetryInterval 间隔重试。
// Redis 服务失效时按兜底策略：FailClosed → 返回错误（不放行临界区，
// 不死循环重试）；FailOpen → 直接返回 nil（放行）。
func (l *Lock) Lock(ctx context.Context) error {
	if l.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.timeout)
		defer cancel()
	}

	for {
		ok, err := l.tryLockRaw(ctx)
		if err != nil {
			if isUnavailable(err) {
				return l.fallbackErr(err)
			}
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
// Redis 服务失效时按兜底策略：FailClosed → 返回错误；FailOpen → 吞错
// 返回 (true, nil)（视为续期成功，放行语义；显式选择的风险由调用方承担）。
func (l *Lock) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("redis: 续期时长必须为正数，got %v", ttl)
	}

	n, err := renewScript.Run(ctx, l.client, []string{l.key}, l.token, ttl.Milliseconds()).Int()
	if err != nil {
		if isUnavailable(err) {
			if l.policy == FailOpen {
				return true, fallbackErr(err)
			}
			return false, fallbackErr(err)
		}
		return false, err
	}
	return n == 1, nil
}
