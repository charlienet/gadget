package redis

import (
	"context"
	"time"

	"github.com/charlienet/gadget/breaker"
	goredis "github.com/redis/go-redis/v9"
)

// CircuitBreaker 是熔断器：状态机实现委托 gadget/breaker
// （三态 Closed/Open/HalfOpen：连续失败达阈值 → Open 快速失败，
// 冷却后半开单飞探测，成功自动恢复）。本类型仅为显式转发 wrapper，
// 导出面与 v0.4.0 完全一致（公共 API 冻结）。
//
// 错误分类注入 IsUnavailable：仅连接/服务类故障计入熔断；命令级错误
// 为中性（不计入、不干扰连续失败计数；半开探测期间视为服务可达 → 闭合）。
//
// 并发安全与冷却惰性判断等实现细节见 gadget/breaker 包文档。
type CircuitBreaker struct {
	b *breaker.Breaker
}

// newCircuitBreaker 创建熔断器（threshold/cooldown 非正时由 breaker.New
// 忽略并保持默认：阈值 3、冷却 1s，与 v0.4.0 语义等价）。
func newCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{b: breaker.New(
		breaker.WithThreshold(threshold),
		breaker.WithCooldown(cooldown),
		breaker.WithClassifier(IsUnavailable),
	)}
}

// Allow 判断请求是否允许执行：nil=放行；拒绝时原样返回最近一次失败错误
// （快速失败，不实际连接）。
func (c *CircuitBreaker) Allow() error { return c.b.Allow() }

// Success 记录成功：重置连续失败计数；半开探测成功 → 闭合恢复。
func (c *CircuitBreaker) Success() { c.b.Success() }

// Fail 记录连接类失败：连续失败达阈值 → Open；半开探测失败 → 回 Open 重置冷却。
func (c *CircuitBreaker) Fail(err error) { c.b.Fail(err) }

// onResult 处理一次命令结果（hook 调用）：委托 breaker.Breaker.Report
// 按 Classifier 三分类（成功 → Success；连接类错误 → Fail；其余中性）。
func (c *CircuitBreaker) onResult(err error) { c.b.Report(err) }

// breakerHook 是接入 go-redis hook 链的熔断 hook。
// 注册顺序关键：go-redis 的 hook 链"后注册的最外层"（withProcessHook 从
// slice 末尾向前包裹），因此熔断 hook 必须在 renameHook **之后**注册，
// 才能位于最外层：先熔断判断（Open 快速失败不执行命令），再前缀改写。
type breakerHook struct {
	breaker *CircuitBreaker
}

func (h *breakerHook) DialHook(next goredis.DialHook) goredis.DialHook {
	// 连接建立由 go-redis 连接池内部管理，直接透传
	return next
}

func (h *breakerHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if err := h.breaker.Allow(); err != nil {
			return err // 快速失败：不执行命令（含前缀改写）
		}
		err := next(ctx, cmd)
		h.breaker.onResult(err)
		return err
	}
}

func (h *breakerHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if err := h.breaker.Allow(); err != nil {
			return err
		}
		err := next(ctx, cmds)
		// 管道整体统计：最后一个错误判定（IsUnavailable 才计入熔断）
		h.breaker.onResult(err)
		return err
	}
}
