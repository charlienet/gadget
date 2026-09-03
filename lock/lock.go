// Package lock 提供基于后端抽象的分布式锁。
//
// 核心 Lock 与具体存储解耦：后端只需实现最小原子原语（占锁 / 校验删，
// 可选续期），正确性逻辑（token、重试、失效兜底）集中在本包。对外能力：
//   - TryLock：非阻塞尝试获取；
//   - Lock：阻塞获取，支持重试间隔与总超时；
//   - Unlock：释放锁；
//   - Renew：续期（后端实现 Renewer 时可用，供看门狗使用）。
//
// 每个锁实例持有唯一 token，释放与续期仅当 key 当前值等于本 token 时才生效，
// 从而防止误删 / 误续他人锁。后端不可用时按 FailPolicy 兜底：默认 FailClosed
// 拒绝放行，可选 FailOpen 放行（风险由调用方承担）。
//
// 典型用法：
//
//	l := lock.New("order:123",
//		lock.WithBackend(myBackend),      // 注入 lock.Backend
//		lock.WithTTL(10*time.Second),     // 锁租约时长
//	)
//
//	ok, err := l.TryLock(ctx)
//	if err != nil {
//		return err
//	}
//	if !ok {
//		return errors.New("锁被占用")
//	}
//	defer l.Unlock(ctx)
//	// ... 进入临界区 ...
package lock

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

// Backend 是分布式锁后端的最小原语契约：核心 Lock 的全部正确性
// 仅依赖这两个原子操作，任何提供原子"占锁/校验删"能力的存储均可实现。
//
// 错误契约（实现者必须遵守）：
//   - 后端自身不可用（连接失败、服务宕机等）时，必须将错误包装为
//     ErrBackendUnavailable（fmt.Errorf("%w: %v", ...)），核心据此执行
//     FailPolicy 兜底；
//   - 其余错误（参数错、命令级错）原样返回，核心原样透传，不兜底；
//   - ctx 取消/超时产生的错误不得包装为 ErrBackendUnavailable。
type Backend interface {
	// TryAcquire 原子尝试获取锁：仅当 key 不存在时写入 token 并设置 ttl。
	// 返回 (true, nil) 表示获取成功；(false, nil) 表示锁已被他人持有
	//（不是错误，是正常互斥结果）；错误仅用于后端故障。
	TryAcquire(ctx context.Context, key, token string, ttl time.Duration) (bool, error)

	// Release 原子释放锁：仅当 key 当前值为 token 时释放（删除/撤销租约）。
	// token 不匹配时静默返回 nil（锁已丢失或被他人持有，无副作用）。
	Release(ctx context.Context, key, token string) error
}

// Renewer 是可选的后端续期能力。后端若不支持原子续期，可以不实现本接口；
// 此时核心 Lock.Renew 返回 (false, ErrRenewUnsupported)。
type Renewer interface {
	// Renew 原子续期：仅当锁仍由 token 持有（key 当前值 == token，
	// 或等价租约归属校验）时，将剩余有效期延长至 ttl。
	// 返回 (true, nil) 续期成功；(false, nil) 锁已丢失（过期或被他人获取）。
	Renew(ctx context.Context, key, token string, ttl time.Duration) (bool, error)
}

// ErrRenewUnsupported 表示后端未实现 Renewer，无法续期。
var ErrRenewUnsupported = errors.New("lock: backend does not support renew")

// Lock 是基于后端原语的分布式锁：公共核心逻辑（token、重试、兜底）
// 与后端解耦，后端只提供原子原语。
type Lock struct {
	backend Backend
	renewer Renewer // 可选续期能力；nil 表示后端不支持

	key     string
	token   string
	ttl     time.Duration
	retry   time.Duration
	timeout time.Duration
	policy  FailPolicy
}

// New 创建分布式锁。backend 必须通过 WithBackend 注入，缺失时 panic。
// 失效兜底策略默认 FailClosed。
func New(key string, opts ...Option) *Lock {
	cfg := defaultOptions()
	for _, o := range opts {
		o(cfg)
	}
	if cfg.backend == nil {
		panic("lock: WithBackend 未配置后端")
	}
	l := &Lock{
		backend: cfg.backend,
		key:     key,
		token:   cfg.token,
		ttl:     cfg.ttl,
		retry:   cfg.retryInterval,
		timeout: cfg.timeout,
		policy:  cfg.policy,
	}
	if l.token == "" {
		l.token = randomToken()
	}
	if r, ok := cfg.backend.(Renewer); ok {
		l.renewer = r
	}
	return l
}

// Token 返回本锁实例的 token（外部指定或随机生成）。
func (l *Lock) Token() string { return l.token }

// TryLock 非阻塞尝试获取锁。返回 true 表示获取成功；false 表示锁已被
// 他人持有。后端不可用时按兜底策略：FailClosed → (false, wrapUnavailable)；
// FailOpen → (true, wrapUnavailable)。
func (l *Lock) TryLock(ctx context.Context) (bool, error) {
	ok, err := l.backend.TryAcquire(ctx, l.key, l.token, l.ttl)
	if err != nil {
		if errors.Is(err, ErrBackendUnavailable) {
			return l.fallbackBool(), wrapUnavailable(err)
		}
		return false, err
	}
	return ok, nil
}

// Unlock 释放锁。token 不匹配时后端静默返回 nil。
// 后端不可用时：FailClosed → wrapUnavailable；FailOpen → nil。
func (l *Lock) Unlock(ctx context.Context) error {
	if err := l.backend.Release(ctx, l.key, l.token); err != nil {
		if errors.Is(err, ErrBackendUnavailable) {
			if l.policy == FailOpen {
				return nil
			}
			return wrapUnavailable(err)
		}
		return err
	}
	return nil
}

// Lock 阻塞获取锁，直到成功、ctx 取消或 WithTimeout 超时，
// 失败后按 WithRetryInterval 间隔重试。
// 后端不可用时：FailClosed → wrapUnavailable；FailOpen → nil。
func (l *Lock) Lock(ctx context.Context) error {
	if l.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.timeout)
		defer cancel()
	}
	timer := time.NewTimer(l.retry)
	defer timer.Stop()
	for {
		ok, err := l.backend.TryAcquire(ctx, l.key, l.token, l.ttl)
		if err != nil {
			if errors.Is(err, ErrBackendUnavailable) {
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
		case <-timer.C:
			timer.Reset(l.retry)
		}
	}
}

// Renew 续期锁（看门狗用）。ttl 必须为正数。
// 后端不支持续期时返回 (false, ErrRenewUnsupported)。
// 后端不可用时：FailClosed → (false, wrapUnavailable)；FailOpen → (true, wrapUnavailable)。
func (l *Lock) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("lock: 续期时长必须为正数，got %v", ttl)
	}
	if l.renewer == nil {
		return false, ErrRenewUnsupported
	}
	ok, err := l.renewer.Renew(ctx, l.key, l.token, ttl)
	if err != nil {
		if errors.Is(err, ErrBackendUnavailable) {
			if l.policy == FailOpen {
				return true, wrapUnavailable(err)
			}
			return false, wrapUnavailable(err)
		}
		return false, err
	}
	return ok, nil
}

// randomToken 生成 16 字符随机 token，字符表为 [a-zA-Z0-9]（共 62 个）。
//
// 用拒绝采样消除取模偏差：0..255 不是 62 的整数倍，直接 b%62 会让前若干字符
// 概率略高。248 = 62*4 是 62 在字节值域内的最大整数倍，故丢弃 >= 248 的字节
// 重采样，剩下的 248 个值可均匀映射到 62 个字符。
//
// crypto/rand.Read 的 error 属系统熵池不可用的环境级故障，token 生成无法继续，
// 此处 fail-fast panic（不存在有意义的回退：时间戳不随机、降级会破坏锁的互斥语义）。
func randomToken() string {
	const (
		letters     = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		table       = len(letters) // 62
		rejectAbove = 248          // 62*4，字节值 >= 此值时丢弃以消除模偏差
		size        = 16
	)

	out := make([]byte, size)
	i := 0
	for i < size {
		var buf [size]byte
		if _, err := rand.Read(buf[:]); err != nil {
			// 系统熵池不可用属环境级故障，token 生成无法继续，fail-fast。
			panic(fmt.Sprintf("lock: crypto/rand 读取失败，无法生成随机 token: %v", err))
		}
		for _, b := range buf {
			if int(b) >= rejectAbove {
				continue
			}
			out[i] = letters[int(b)%table]
			i++
			if i == size {
				break
			}
		}
	}
	return string(out)
}
