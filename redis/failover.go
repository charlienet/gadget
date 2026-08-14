package redis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
)

// FailPolicy 定义 Redis 服务失效时的兜底策略。
type FailPolicy uint8

const (
	// FailClosed 失效时拒绝（返回失败值）：适用于锁等"宁可失败也不放行"的
	// 能力——服务不可用时放行临界区会造成并发写数据。
	FailClosed FailPolicy = iota
	// FailOpen 失效时放行（返回成功值）：适用于限流、过滤器等保护性能力，
	// 服务不可用时宁可多放也不阻塞业务。
	FailOpen
)

// isUnavailable 判定 err 是否为"Redis 服务不可用"类错误（连接/服务层故障），
// 这类错误才触发兜底；命令级错误（WRONGTYPE、语法错误、业务语义错误等）
// 必须照常返回，不能兜底吞掉。
//
// 判定依据（go-redis v9.22 源码确认：无公开 IsTimeout/IsNetworkError，
// 错误形态如下）：
//   - context.Canceled / context.DeadlineExceeded：调用方主动取消或自身
//     ctx 超时约束，**不代表 Redis 失效，不触发兜底**（ctx 取消是调用方行为）
//   - "redis: connection pool timeout" / "redis: client is closed"：
//     连接池超时 / 连接池已关闭（internal/pool 的错误文本）
//   - *net.OpError：dial 失败、连接重置等网络层错误
//   - io.EOF / io.ErrUnexpectedEOF：连接被服务端关闭
//   - net.Error（Timeout() 为 true）：读写超时（服务端未响应），但排除
//     context.DeadlineExceeded（调用方超时约束，非服务失效）
func isUnavailable(err error) bool {
	if err == nil {
		return false
	}

	// 调用方主动取消 / 自身 ctx 超时约束：不触发兜底
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// 连接池故障（internal/pool.ErrPoolTimeout / ErrClosed 的文本形态）
	msg := err.Error()
	if strings.Contains(msg, "redis: connection pool timeout") ||
		strings.Contains(msg, "redis: client is closed") {
		return true
	}

	// 网络层错误：dial 失败、连接重置等
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// 连接被服务端关闭
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// 读写超时（服务端未响应）；ctx 超时已在前面排除
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	return false
}

// ErrRedisUnavailable 表示 Redis 服务不可用、已按 FailPolicy 执行兜底。
// 各扩展兜底生效时返回该哨兵错误（包装原始错误，errors.Is 可命中），
// 应用层可感知脱机事件并自行处理（告警、降级、重试策略等）。
//
// 用法示例：
//
//	ok, err := lock.TryLock(ctx)
//	if errors.Is(err, redis.ErrRedisUnavailable) {
//		// Redis 不可用，锁已按策略兜底（默认 FailClosed：ok=false）
//		notifyAlert() // 自行处理：告警/降级
//	}
var ErrRedisUnavailable = errors.New("redis: server unavailable")

// fallbackErr 包装原始错误为兜底哨兵错误：errors.Is(err, ErrRedisUnavailable)
// 可命中，同时保留原始错误信息（err.Error() 含底层原因，便于排查）。
func fallbackErr(err error) error {
	return fmt.Errorf("%w: %v", ErrRedisUnavailable, err)
}

// failPolicyConfig 内嵌于各扩展的 config（LockOption/BloomOption 等对应的
// 配置结构），统一失效兜底策略字段与设置方法。
type failPolicyConfig struct {
	policy FailPolicy
}

// setPolicy 设置失效兜底策略（指针接收者，各扩展 config 内嵌后方法提升）。
func (c *failPolicyConfig) setPolicy(p FailPolicy) {
	c.policy = p
}

// failPolicySetter 是各扩展 config 类型需满足的接口（内嵌 failPolicyConfig
// 后自动满足），用作 WithFailPolicy 的泛型约束。
type failPolicySetter interface {
	setPolicy(FailPolicy)
}

// WithFailPolicy 设置 Redis 服务失效时的兜底策略，返回对应扩展的 Option。
// Go 不支持同名函数重载，各扩展的 Option 是不同类型
// （LockOption/BloomOption/CuckooOption/LeakyBucketOption/...），因此以
// 泛型提供统一入口：类型参数 C 为扩展的 config 类型（指针实现 setPolicy）。
//
// 用法（配合各扩展导出的 config 类型别名）：
//
//	// 锁：默认 FailClosed，可显式 FailOpen（警告：失效放行临界区有并发风险）
//	lock := rdb.NewLock("k", redis.WithFailPolicy[*redis.LockConfig](redis.FailClosed))
//	// 限流器/过滤器：默认 FailOpen
//	rl := rdb.NewRateLimiter("login", redis.WithFailPolicy[*redis.RateLimiter](redis.FailOpen))
//	cf := rdb.NewCuckooFilter("cf:1", redis.WithFailPolicy[*redis.CuckooConfig](redis.FailOpen))
func WithFailPolicy[C failPolicySetter](policy FailPolicy) func(C) {
	return func(c C) {
		c.setPolicy(policy)
	}
}
