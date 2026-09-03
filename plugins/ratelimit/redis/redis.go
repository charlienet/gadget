// Package redis 提供 ratelimit.Backend 的 Redis 实现：以 GCRA 令牌桶脚本
// 作为"远程批发通道"，速率配置随 ratelimit.Spec 逐次下发，本包不自带
// 任何速率配置（后端无状态，杜绝双配置源）。
//
// 脚本以 gadget/redis 模块的 tokenBucketAtMostScript 为基础复制改造
// （原脚本属冻结的公共 API，不动）；burst 与 rate 解耦、新增 AllOrNothing
// 分支、BestEffort 裁剪按 floor 推进（扣减量==返回量）。两份脚本内容同源，
// 修改任何一方的公共语义时须互相同步，详见 script.go 注释。
//
// 错误契约（对齐 lock.Backend 三条，ratelimit 核心据此分诊）：
//   - 连接/拨码/服务类故障包装为 ratelimit.ErrBackendUnavailable，
//     交由 core 按 FailPolicy 兜底；
//   - 命令级错误（如 Lua 运行错误）原样透传，不兜底——防配置错误被掩盖；
//   - ctx 取消/超时原样返回 ctx.Err()，绝不包装为不可用。
//
// 由于包名 redis 与 go-redis 冲突，import 时建议起别名 redislimit：
//
//	import (
//		goredis "github.com/redis/go-redis/v9"
//		redislimit "github.com/charlienet/gadget/plugins/ratelimit/redis"
//		"github.com/charlienet/gadget/ratelimit"
//	)
//
//	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379"})
//	limiter := ratelimit.New(redislimit.New(rdb),
//		ratelimit.WithRate(100, time.Minute),
//		ratelimit.WithBurst(200),
//	)
//	defer limiter.Close() // 会经本包 Close 释放 rdb 连接资源
//
// 多实例部署时各实例创建相同的 Limiter 配置即共享同一份 Redis 全局配额；
// 租约模式下全局瞬时突发上界为 (实例数+1)×Burst（见 ratelimit 包 doc 披露）。
package redis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/charlienet/gadget/ratelimit"
)

var (
	_ ratelimit.Backend = (*Backend)(nil)
	_ io.Closer         = (*Backend)(nil)
)

// defaultKeyPrefix 与 gadget/redis 模块 RateLimiter 的 key 命名空间一致
// （"rate:"），便于从旧实现平滑迁移时复用既有状态。
const defaultKeyPrefix = "rate:"

// Backend 是 Redis 批发后端，实现 ratelimit.Backend（可选 io.Closer）。
type Backend struct {
	rdb    goredis.Cmdable
	prefix string
}

// Option 自定义 Backend 的构造选项。
type Option func(*Backend)

// New 创建 Redis 限流后端。client 必传，nil 时 panic（fail-fast，对齐
// plugins/lock/redis 与 ratelimit.New 先例）。
//
// 不设拨码/读写超时类 Option：那属于 go-redis client 自身的配置职责（N3）；
// ratelimit core 会以 WithBackendTimeout 约束每次批发的内部 ctx。
func New(client *goredis.Client, opts ...Option) ratelimit.Backend {
	if client == nil {
		panic("ratelimit/redis: nil redis client")
	}
	b := &Backend{rdb: client, prefix: defaultKeyPrefix}
	for _, o := range opts {
		o(b)
	}
	return b
}

// WithKeyPrefix 覆盖限流 key 的命名空间前缀（默认 "rate:"）。
// 空字符串视为非法值，防御式忽略保持默认（对齐 core Option 风格）。
func WithKeyPrefix(prefix string) Option {
	return func(b *Backend) {
		if prefix != "" {
			b.prefix = prefix
		}
	}
}

// limitKey 组合前缀与业务 key。
func (b *Backend) limitKey(key string) string {
	return b.prefix + key
}

// Wholesale 为 key 按 spec 批量申请 want 个令牌（GCRA 原子扣减）。
//
// 返回值映射：granted = 脚本实际消耗的 cost；retryAfter = 未足额时脚本
// 提示的最早重试时刻（足额时为 0）。错误分诊见包 doc 三条契约。
func (b *Backend) Wholesale(ctx context.Context, key string, want int, spec ratelimit.Spec, mode ratelimit.GrantMode) (int, time.Duration, error) {
	// 契约三条之 ctx：进入前已取消直接透传，绝不进不可用分诊。
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}

	v, err := wholesaleScript.Run(ctx, b.rdb, []string{b.limitKey(key)},
		spec.Burst, spec.Rate, spec.Per.Seconds(), want, int(mode)).Result()
	if err != nil {
		// 调用期间 ctx 才取消/超时：go-redis 会把 ctx 错误与拨码错误混发，
		// 以 ctx.Err() 为准原样透传（契约三条之 ctx）。
		if e := ctx.Err(); e != nil {
			return 0, 0, e
		}
		if isUnavailable(err) {
			return 0, 0, fmt.Errorf("%w: %v", ratelimit.ErrBackendUnavailable, err)
		}
		return 0, 0, err // 命令级错误（含 Lua 运行错误）原样透传，不兜底
	}

	granted, retryAfter, perr := parseWholesaleResult(v)
	if perr != nil {
		return 0, 0, perr
	}
	return granted, retryAfter, nil
}

// Close 释放底层连接资源（实现 io.Closer，供 ratelimit.Limiter.Close 转调）。
// 仅做资源释放，不涉及任何令牌归还语义（本包不做 giveback）。
func (b *Backend) Close() error {
	if c, ok := b.rdb.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// parseWholesaleResult 映射脚本返回
// {实际消耗 cost(int), remaining(int 截断), retry_after(秒字符串), reset_after(秒字符串)}。
// 结构异常视为后端协议错误原样报错（不包装 ErrBackendUnavailable：
// 这是返回值语义问题，不是服务不可用）。
func parseWholesaleResult(v any) (int, time.Duration, error) {
	values, ok := v.([]any)
	if !ok || len(values) < 4 {
		return 0, 0, fmt.Errorf("ratelimit/redis: 批发脚本返回异常结构: %v", v)
	}
	granted, ok := values[0].(int64)
	if !ok {
		return 0, 0, fmt.Errorf("ratelimit/redis: 批发脚本返回的 granted 无法解析为整数: %v", values[0])
	}
	// 放行时 retry_after 为 "-1" 占位，仅被拒时为正（秒，字符串保完整精度，
	// 对齐 redis/ratelimit.go 的解析约定）。
	retryAfter, err := parseSeconds(values[2])
	if err != nil {
		return 0, 0, err
	}
	if granted < 0 {
		granted = 0
	}
	return int(granted), retryAfter, nil
}

// parseSeconds 把脚本返回的秒数字符串解析为 Duration；<=0 归 0。
func parseSeconds(v any) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("ratelimit/redis: 批发脚本返回的时间字段无法解析为字符串: %v", v)
	}
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("ratelimit/redis: 解析 retry_after 失败: %w", err)
	}
	if sec <= 0 {
		return 0, nil
	}
	return time.Duration(sec * float64(time.Second)), nil
}

// isUnavailable 判定 err 是否为"Redis 服务不可用"类错误（连接/服务层故障）。
// 与 github.com/charlienet/gadget/redis.IsUnavailable 及 plugins/lock/redis
// 的同名函数逻辑一致，此处独立实现以避免引入 gadget/redis 全家桶依赖。
func isUnavailable(err error) bool {
	if err == nil {
		return false
	}
	// 调用方主动取消 / 自身 ctx 超时约束：不触发兜底
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "redis: connection pool timeout") ||
		strings.Contains(msg, "redis: client is closed") {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}
