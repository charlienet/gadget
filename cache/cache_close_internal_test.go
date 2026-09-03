package cache

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// hangingListener 的 Close 仅在 ctx 取消或测试结束（hang 关闭）时返回，
// 模拟"耗时/违约"的第三方 Listener：若调用方传入无 deadline 的 ctx 即永久阻塞。
type hangingListener struct {
	hang chan struct{}
}

func (l *hangingListener) Subscribe() chan string { return make(chan string) }
func (l *hangingListener) Publish(string) error   { return nil }
func (l *hangingListener) Ready() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (l *hangingListener) Close(ctx context.Context) error {
	select {
	case <-ctx.Done(): // 尊重调用方超时（修复后 8s 到）
		return ctx.Err()
	case <-l.hang: // 测试结束释放（红证兜底，防 goroutine 泄漏）
		return nil
	}
}

// quickListener 的 Close 立即返回，用于 goroutine 收敛测试隔离 D1。
type quickListener struct{}

func (quickListener) Subscribe() chan string      { return make(chan string) }
func (quickListener) Publish(string) error        { return nil }
func (quickListener) Ready() <-chan struct{}      { ch := make(chan struct{}); close(ch); return ch }
func (quickListener) Close(context.Context) error { return nil }

// syncProbeStore 记录 syncBatch 传入的 ctx 是否带 deadline；Get 阻塞至 ctx 取消，
// 模拟"慢/无超时纪律下会拖死版本同步"的远程。
type syncProbeStore struct {
	mu          sync.Mutex
	calls       int
	gotDeadline bool
}

func (s *syncProbeStore) Get(ctx context.Context, _ string) ([]byte, bool, error) {
	_, ok := ctx.Deadline()
	s.mu.Lock()
	s.calls++
	if ok {
		s.gotDeadline = true
	}
	s.mu.Unlock()
	<-ctx.Done() // 阻塞直到超时；无 deadline 的 ctx（D2 未修复）会永久阻塞
	return nil, false, ctx.Err()
}
func (s *syncProbeStore) Put(context.Context, string, []byte, int) error { return nil }
func (s *syncProbeStore) Delete(context.Context, ...string) error        { return nil }
func (s *syncProbeStore) Name() string                                   { return "sync-probe" }
func (s *syncProbeStore) IsRemote() bool                                 { return true }

// D1：listener.Close 无界阻塞时，cache.Close 必须在超时+裕量内有界返回。
// 修复前：startWatcher 用 context.Background() 调 Close → 无超时 → watcher 不退出
// → watcherWG.Wait 押死 → cache.Close 永久挂起（看门狗触发 → 红）。
// 修复后：Close 传入 listenerCloseTimeout(8s) 的 ctx → hangingListener 在 8s 被
// ctx 打断返回 → cache.Close ~8s 返回 < 11s 阈值（绿）。
func TestCloseBoundedWhenListenerCloseHangs(t *testing.T) {
	remote := newTestRemoteStore()
	hang := make(chan struct{})
	defer close(hang) // 测试结束释放，避免红证路径下的 goroutine 泄漏
	lis := &hangingListener{hang: hang}

	c := newDelayCache(t, 0, remote, WithListener(lis))

	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()

	select {
	case <-done:
		// 有界返回（修复后 ~8s）。
	case <-time.After(11 * time.Second): // listenerCloseTimeout(8s) + 3s 裕量
		t.Fatal("cache.Close 未在 11s 内有界返回：listener.Close 无超时挂死（D1）")
	}
}

// 后台选项全开实例：New 前后 goroutine 数对比 + Close 后有界收敛。
// 用 quickListener 隔离 D1（其 Close 立即返回）。容差吸收同包其他用例前序实例的
// 短暂残留（各实例 Close 后 ≤3s 收敛）；以 -count=3 稳定为准。
func TestCloseStopsAllBackgroundGoroutines(t *testing.T) {
	const tolerance = 40

	runtime.GC()
	base := runtime.NumGoroutine()

	local := newMemStore()
	remote := newTestRemoteStore()
	c := New(
		func(o *Options) { o.WithStore(local) },
		func(o *Options) { o.WithStore(remote) },
		WithListener(quickListener{}),
		WithDegradeThreshold(1),
		WithDegradeRecoveryInterval(20*time.Millisecond),
		WithVersionSyncInterval(20*time.Millisecond),
		WithCleanupInterval(20*time.Millisecond),
	)

	time.Sleep(50 * time.Millisecond) // 让 healthLoop / versionSyncLoop / startWatcher / janitor 全部起来
	peak := runtime.NumGoroutine()
	assert.Greater(t, peak, base, "New 后应启动多个后台 goroutine（health/versionSync/watcher/janitor）")

	c.Close()

	deadline := time.Now().Add(3 * time.Second)
	var after int
	for {
		after = runtime.NumGoroutine()
		if after <= base+tolerance || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.LessOrEqual(t, after, base+tolerance, "Close 后后台 goroutine 应在 3s 内有界收敛回基线附近")
}

// 契约锁定：请求 ctx 已取消，二次删仍应执行——它用自建 Background+flushTimeout ctx。
func TestDelayedSecondDeleteNotCancelledByRequestCtx(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 30*time.Millisecond, remote)

	cctx, cancel := context.WithCancel(context.Background())
	cancel() // 请求 ctx 立即取消
	assert.NoError(t, c.Invalidate(cctx, mutateKeys("k")))

	// 竞态回填 L2 旧值。
	assert.NoError(t, remote.Put(context.Background(), "k", makeVersionedData(100, []byte("stale")), 60))

	ok := delayEventually(time.Second, 5*time.Millisecond, func() bool {
		return !delayRemoteHas(remote, "k")
	})
	assert.True(t, ok, "二次删用自建 ctx，应无视已取消的请求 ctx 正常补删（契约）")
}

// D2：syncBatch 必须给 remote 一个带超时的 ctx，慢 Store 不得拖死版本同步循环。
func TestSyncBatchAppliesTimeoutToRemoteGet(t *testing.T) {
	probe := &syncProbeStore{}
	local := newMemStore()
	_ = local.Put(context.Background(), "key", makeVersionedData(100, []byte("x")), 60)

	c := &cache{
		localStore:       local,
		remoteStore:      probe,
		ttl:              60,
		versionSyncBatch: defaultVersionSyncBatch,
		logger:           slog.Default(),
	}

	done := make(chan struct{})
	start := time.Now()
	go func() {
		c.syncBatch()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("syncBatch 未在超时上界内返回：传给 remote 的 ctx 无 deadline，慢 Store 拖死版本同步（D2）")
	}

	elapsed := time.Since(start)
	assert.Less(t, elapsed, 4500*time.Millisecond, "一轮 syncBatch 应在 flushTimeout 附近返回")

	probe.mu.Lock()
	assert.True(t, probe.gotDeadline, "传给 remote 的 ctx 应带 deadline（flushTimeout）")
	assert.GreaterOrEqual(t, probe.calls, 1)
	probe.mu.Unlock()
}
