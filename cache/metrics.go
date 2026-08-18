package cache

// Metrics defines the observability interface for cache events.
// Users can provide a custom implementation (Prometheus, OTEL, etc.)
// via WithMetrics(). The default implementation is a no-op.
//
// 实现必须并发安全：降级状态切换（SetDegraded）由 healthLoop 与请求路径的
// 多个 goroutine 驱动，CacheEviction 由各 store 的后台清理 goroutine 调用。
type Metrics interface {
	// CacheEviction is called when an entry is evicted from the local store
	// due to capacity limits.
	CacheEviction()

	// SetDegraded is called when the cache enters or exits degraded mode
	// (remote store unavailable).
	SetDegraded(on bool)
}

type noopMetrics struct{}

func (noopMetrics) CacheEviction()   {}
func (noopMetrics) SetDegraded(bool) {}
