package redis

import "context"

// prefillFilter 是启用 WithPrefill 后的 BloomFilter 装饰器：包住
// bfCmdImpl/bitmapImpl，按本地新鲜 Ready 相位分派数据面，拦截 Reset
// （手工触发重建的唯一入口）、State，并持有 prefillCoordinator
// （后台同步 + 惰性重建）。
//
// FailPolicy 正交声明：状态明确未就绪时 Exists/ExistsMulti 恒 true 是
// 业务规则（门控降级），不受 FailPolicy 影响；FailPolicy 只作用于
// Ready 态透传后的既有数据面路径（inner 自身的失效兜底语义不变）。
// Info/Card 是观测类，恒透传 inner 不降级。
type prefillFilter struct {
	inner BloomFilter
	coord *prefillCoordinator
}

// newPrefillFilter 组装装饰器并启动协调器（coordinator 注册进
// closeState 级联，GracefulClose 时停 ticker；亦可 Close 单独释放）。
// 调用方须已确认 cfg.enabled && cfg.fn != nil 且 inner.connectAll 成功。
func newPrefillFilter(rdb *redisClient, inner BloomFilter, base string, cfg bloomConfig) *prefillFilter {
	return &prefillFilter{
		inner: inner,
		// 工厂契约：inner 恒为裸 impl（*bfCmdImpl/*bitmapImpl，均满足
		// prefillInner——G1 后含 IntegrityProbe），断言安全。
		coord: newPrefillCoordinator(rdb, inner.(prefillInner), base, cfg.prefillConfig),
	}
}

// Close 释放协调器（停后台同步 ticker），幂等；不进 BloomFilter 接口，
// 供单独释放装饰器持有的后台资源。
func (f *prefillFilter) Close() error {
	f.coord.Close()
	return nil
}

// maybeTrigger 热路径惰性触发：本地非新鲜 Ready（含 stale，T10）时经
// in-flight CAS 起后台 force=0 重建（冷启动首请求即触发）；零 RTT，
// 不阻塞当前请求。
func (f *prefillFilter) maybeTrigger(ctx context.Context) {
	f.coord.triggerLazy(ctx)
}

// Add 数据面：恒透传 inner 写入放行（非 Ready 也照常写入，清空窗口内的
// 增量由回灌补回）；同时可能惰性触发重建。
func (f *prefillFilter) Add(ctx context.Context, item any) (bool, error) {
	f.maybeTrigger(ctx)
	return f.inner.Add(ctx, item)
}

// AddMulti 数据面批量写：语义同 Add（照常透传 + 惰性触发）。
func (f *prefillFilter) AddMulti(ctx context.Context, items ...any) ([]bool, error) {
	f.maybeTrigger(ctx)
	return f.inner.AddMulti(ctx, items...)
}

// Exists 查询面：非新鲜 Ready（含 stale）恒返回 (true, nil)——门控降级
// 的业务规则，不经 FailPolicy；Ready 透传 inner 真实查询。
func (f *prefillFilter) Exists(ctx context.Context, item any) (bool, error) {
	f.maybeTrigger(ctx)
	if !f.coord.readyFresh() {
		return true, nil
	}
	return f.inner.Exists(ctx, item)
}

// ExistsMulti 批量查询：非新鲜 Ready 恒全 true + nil；Ready 透传。
// 空入参直接透传 inner（R4：两态返回一致，非 Ready 也返回 nil 而非
// 空非 nil 切片）。
func (f *prefillFilter) ExistsMulti(ctx context.Context, items ...any) ([]bool, error) {
	if len(items) == 0 {
		return f.inner.ExistsMulti(ctx, items...)
	}
	f.maybeTrigger(ctx)
	if !f.coord.readyFresh() {
		out := make([]bool, len(items))
		for i := range out {
			out[i] = true
		}
		return out, nil
	}
	return f.inner.ExistsMulti(ctx, items...)
}

// Info 观测类：恒透传 inner（不降级、不触发重建）。
func (f *prefillFilter) Info(ctx context.Context) (*BloomInfo, error) {
	return f.inner.Info(ctx)
}

// Card 观测类：恒透传 inner（不降级、不触发重建）。
func (f *prefillFilter) Card(ctx context.Context) (int64, error) {
	return f.inner.Card(ctx)
}

// Reset 拦截（规格 §6）：启用预填充后是手工触发重建的唯一入口，等价
// force=1 的重建全流程（抢占 → Δ → 清空重建 → fn 回灌 → ready/fail），
// 同步执行直至返回；Building 中抢不到锁返回 ErrRebuildInProgress。
// 启用后错误原样返回（ErrRebuildInProgress / ErrRebuildTimeout /
// cause），不经 fallbackErr 哨兵包装。
// 未启用保持原语义（纯清空），由工厂不包装保证。
func (f *prefillFilter) Reset(ctx context.Context) error {
	return f.coord.run(ctx, 1)
}

// State 1 RTT 直接 GET 权威状态键（不经本地缓存）。
func (f *prefillFilter) State(ctx context.Context) (PrefillPhase, error) {
	return f.coord.readState(ctx)
}

// Phase 零 RTT 纯内存读本地相位快照（委托 coordinator 组装；接口契约与
// State 的分工线见 BloomFilter.Phase / PhaseInfo godoc）。恒返回 nil 错
// 误——启用预填充后快照恒可读（构造即有 Uninitialized 初值），同步故障
// 经 PhaseInfo.LastSyncErr 承载而非返回值。
func (f *prefillFilter) Phase() (PhaseInfo, error) {
	return f.coord.phaseInfo(), nil
}

// IntegrityProbe 透传委托 inner 的 G1 校验（装饰器不拦截观测类调用；
// coordinator 搭车路径直接用 inner，不经本方法）。工厂契约保证 inner
// 恒为裸 impl（bfCmdImpl/bitmapImpl，均满足 prefillInner——与
// coordinator.inner 同一实例）。
func (f *prefillFilter) IntegrityProbe(ctx context.Context) (bool, error) {
	return f.inner.(prefillInner).IntegrityProbe(ctx)
}
