package logger

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
)

// registryCount 当前包级注册表中的实例数（测试隔离用：只比较增量）
func registryCount() int {
	loggerMu.Lock()
	defer loggerMu.Unlock()
	return len(loggerList)
}

// --- M-2：New 替换默认实例前关闭旧实例（flush 异步队列 + 注册表不累积）---

func TestNewClosesPreviousDefault(t *testing.T) {
	// 保存/恢复包级默认状态，避免污染后续测试
	defaultMu.Lock()
	prevLogger, prevLeveler, prevInst := DefaultLogger, defaultLeveler, defaultInstance
	defaultMu.Unlock()
	t.Cleanup(func() {
		defaultMu.Lock()
		DefaultLogger, defaultLeveler, defaultInstance = prevLogger, prevLeveler, prevInst
		defaultMu.Unlock()
	})

	// 实例 A：异步 + buffer 输出（模拟一个持有未落盘队列的旧默认实例）
	var bufA bytes.Buffer
	lA := New(WithOutput(&bufA), WithAsync(64), WithColor(false))
	_ = lA
	lA.Info("flushed by replacement")

	// 实例 B：再 New 应同步关闭 A —— 返回时 A 队列必已排空
	lB := New(WithOutput(io.Discard), WithColor(false))

	gotA := bufA.String()
	if !strings.Contains(gotA, "flushed by replacement") {
		t.Errorf("expected previous default's async queue flushed on replacement, got: %q", gotA)
	}

	// 注册表不累积：A 已 close+注销；注册表内容即 B（不枚举比较，避免依赖其他测试残留实例）
	if got := registrySnapshot(); len(got) != 1 || got[0].slog != lB {
		t.Errorf("expected registry to hold exactly the new default B, got %d entries", len(got))
	}

	// 清理 B（当前 defaultInstance），注册表回到 base
	defaultMu.Lock()
	inst := defaultInstance
	defaultMu.Unlock()
	if inst != nil {
		_ = inst.close(0)
	}
	for _, v := range registrySnapshot() {
		if v.slog == lB {
			t.Error("expected B unregistered after close")
		}
	}
}

// registrySnapshot 返回注册表内容快照（仅测试断言用）
func registrySnapshot() []*slogLogger {
	loggerMu.Lock()
	defer loggerMu.Unlock()
	out := make([]*slogLogger, len(loggerList))
	copy(out, loggerList)
	return out
}

// --- M-5：defaultMu 下并发改写默认引用不 panic / 注册表操作互斥 ---
// （行为级冒烟：并发 New 的 close-previous + register 组合路径，完整 -race
// 验证见外部 TestConcurrentDefaultLifecycle。）

func TestConcurrentNewRegistryChurn(t *testing.T) {
	defaultMu.Lock()
	prevInst := defaultInstance
	prevLeveler := defaultLeveler
	defaultMu.Unlock()
	t.Cleanup(func() {
		defaultMu.Lock()
		defaultLeveler = prevLeveler
		defaultMu.Unlock()
	})
	_ = prevInst

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_ = New(WithOutput(io.Discard), WithColor(false))
			}
		}()
	}
	wg.Wait()

	// 无论并发顺序如何，最终注册表增量至多为 1（最后写入者），
	// 每个被替换的实例都已 close/注销；清掉当前默认实例恢复现场
	defaultMu.Lock()
	inst := defaultInstance
	defaultMu.Unlock()
	if inst != nil {
		_ = inst.close(0)
	}
}
