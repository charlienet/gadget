package logger

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestSamplingCounterGC 验证 A2：counters 定期清理——过期的 counter 在
// 达到 scanInterval 次调用后被移除，防止动态消息长时间运行导致无界增长。
func TestSamplingCounterGC(t *testing.T) {
	h := NewSamplingHandler(slog.NewTextHandler(io.Discard, nil), &SamplingOptions{First: 10, Thereafter: 2})
	sh := h.(*samplingHandler)

	// 注入一个早已过期的 counter
	sh.mu.Lock()
	sh.counters["stale|old"] = &samplerCounter{count: 99, start: time.Now().Add(-2 * time.Hour)}
	sh.mu.Unlock()

	// 触发 scanInterval 次调用（达到扫描阈值）
	for range scanInterval {
		_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "fresh", 0))
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, ok := sh.counters["stale|old"]; ok {
		t.Error("expected stale counter removed after GC scan")
	}
}
