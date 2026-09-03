package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// mutateKeys 返回一个不改数据源、仅声明受影响 key 的 MutateFn，用于测试 Invalidate
// 的失效与二次删调度（keys 原样返回，去重/滤空由库负责）。
func mutateKeys(keys ...string) MutateFn {
	return func(_ context.Context) ([]string, error) { return keys, nil }
}

// delayRemoteHas 报告 testRemoteStore 是否仍持有 key（加锁读，避免与后台二次删竞争）。
func delayRemoteHas(s *testRemoteStore, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[key]
	return ok
}

// localHas 报告底层 store 是否仍持有 key。
func localHas(s Store, key string) bool {
	_, exist, err := s.Get(context.Background(), key)
	return err == nil && exist
}

// delayEventually 在 timeout 内以 interval 轮询 cond，返回是否在超时前满足。
// 用轮询替代固定 sleep，避免定时器抖动导致的 flaky。
func delayEventually(timeout, interval time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

// delayRecordingListener 记录每次 Publish 的 key，用于断言二次删的集群广播。
// Subscribe 返回的 channel 不做回环投递（不影响 watcher），Close 幂等空操作。
type delayRecordingListener struct {
	mu        sync.Mutex
	published []string
}

func (l *delayRecordingListener) Subscribe() chan string { return make(chan string, 16) }
func (l *delayRecordingListener) Publish(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.published = append(l.published, key)
	return nil
}
func (l *delayRecordingListener) Ready() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (l *delayRecordingListener) Close(context.Context) error { return nil }

func (l *delayRecordingListener) count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, k := range l.published {
		if k == key {
			n++
		}
	}
	return n
}

// newDelayCache 构造带 L1（mem_store）+ L2（testRemoteStore）的 cache 实例；
// delay>0 时开启延时二次删除，delay=0 保持默认关闭。extra 追加选项（如 listener）。
// 注册 Cleanup 关闭实例。
func newDelayCache(t *testing.T, delay time.Duration, remote *testRemoteStore, extra ...Option) *cache {
	t.Helper()
	local := newMemStore()
	opts := []Option{
		func(o *Options) { o.WithStore(local) },
		func(o *Options) { o.WithStore(remote) },
	}
	if delay > 0 {
		opts = append(opts, WithDelayedSecondDelete(delay))
	}
	opts = append(opts, extra...)
	c := New(opts...)
	t.Cleanup(c.Close)
	return c
}

// 默认关闭：Invalidate 后不产生任何延迟删除行为。
func TestDelayedSecondDeleteDisabledByDefault(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 0, remote) // 未开启

	assert.Nil(t, c.delayedTimers, "关闭时不应分配 delayedTimers")

	ctx := context.Background()
	assert.NoError(t, c.Put(ctx, "k", "v", 60))
	assert.NoError(t, c.Invalidate(ctx, mutateKeys("k")))

	// 调度入口应早返回，map 仍为 nil。
	assert.Nil(t, c.delayedTimers)

	// 关闭开关后重新写入的值，不应有任何后台二次删清除它。
	assert.NoError(t, c.Put(ctx, "k", "v2", 60))
	time.Sleep(80 * time.Millisecond)
	assert.True(t, delayRemoteHas(remote, "k"), "关闭时新值不应被清除")
}

// 脏回填被清：Invalidate 后向 L2 落入旧值，无条件二次删应清除。
func TestDelayedSecondDeleteClearsStaleL2(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 30*time.Millisecond, remote)
	ctx := context.Background()

	assert.NoError(t, c.Invalidate(ctx, mutateKeys("k")))

	// 模拟竞态：首次删除完成后，迟到的回源把旧值回填进 L2（无条件删不看版本，任何值均清）。
	assert.NoError(t, remote.Put(ctx, "k", makeVersionedData(100, []byte("stale")), 60))

	ok := delayEventually(time.Second, 5*time.Millisecond, func() bool {
		return !delayRemoteHas(remote, "k")
	})
	assert.True(t, ok, "落入 L2 的旧值应被无条件二次删清除")
}

// 无条件删代价 + fail-safe：窗口内落入的合法新值也会被清除，下次 Getfn 回源自愈。
func TestDelayedSecondDeleteClearsWindowWriteAndSelfHeals(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 40*time.Millisecond, remote)
	ctx := context.Background()

	assert.NoError(t, c.Invalidate(ctx, mutateKeys("k"))) // 调度一次无条件二次删

	// 窗口内（二次删触发前）写入合法新值，同时落入 L1+L2。
	assert.NoError(t, c.Put(ctx, "k", "fresh", 60))
	assert.True(t, delayRemoteHas(remote, "k"))

	// 经典无条件二次删：即便晚于失效时刻写入的合法新值也会被一并清除（fail-safe 代价）。
	ok := delayEventually(time.Second, 5*time.Millisecond, func() bool {
		return !delayRemoteHas(remote, "k")
	})
	assert.True(t, ok, "延时二次删应无条件清除窗口内落入的新值")

	// 自愈：下次 Getfn 回源得到正确新值。
	var got string
	err := c.Getfn(ctx, "k", &got, func(_ context.Context, _ string, v any) (bool, error) {
		*(v.(*string)) = "fresh"
		return true, nil
	}, 60)
	assert.NoError(t, err)
	assert.Equal(t, "fresh", got)
	// 回源后新值重新回填 L2，且不再有二次删调度干扰。
	assert.True(t, delayRemoteHas(remote, "k"))
}

// L1 脏值清理分支：竞态回填仅落入 L1，无条件二次删应清除本地副本。
func TestDelayedSecondDeleteClearsL1(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 30*time.Millisecond, remote)
	ctx := context.Background()

	assert.NoError(t, c.Invalidate(ctx, mutateKeys("k")))

	// 竞态回填仅落 L1。
	assert.NoError(t, c.localStore.Put(ctx, "k", makeVersionedData(100, []byte("stale")), 60))
	assert.True(t, localHas(c.localStore, "k"))

	ok := delayEventually(time.Second, 5*time.Millisecond, func() bool {
		return !localHas(c.localStore, "k")
	})
	assert.True(t, ok, "延时二次删应无条件清除 L1 脏值")
}

// 降级跳过分支：二删遇 remote 降级则跳过 L2、不入 pendingDeletes、恢复后 flush 不补删；
// L1 仍被清且广播仍发生。
func TestDelayedSecondDeleteSkipsRemoteWhenDegraded(t *testing.T) {
	remote := newTestRemoteStore()
	lis := &delayRecordingListener{}
	c := newDelayCache(t, 40*time.Millisecond, remote, WithListener(lis))
	ctx := context.Background()

	// 首次失效在正常态完成：双删走 remote，不进 pendingDeletes。
	assert.NoError(t, c.Invalidate(ctx, mutateKeys("k")))

	// 首删后进入降级，再模拟竞态回填 L1/L2。
	c.degraded.Store(true)
	assert.NoError(t, remote.Put(ctx, "k", makeVersionedData(100, []byte("stale")), 60))
	assert.NoError(t, c.localStore.Put(ctx, "k", makeVersionedData(100, []byte("stale")), 60))

	// 二删触发：L1 无条件清除；L2 因降级跳过。
	ok := delayEventually(time.Second, 5*time.Millisecond, func() bool {
		return !localHas(c.localStore, "k")
	})
	assert.True(t, ok, "降级下 L1 仍应被二次删清除")
	time.Sleep(20 * time.Millisecond) // 越过 delay，确认 L2 未被误删
	assert.True(t, delayRemoteHas(remote, "k"), "降级期应跳过 L2 补删")

	// 二次删未把该 key 塞进 pendingDeletes（绕过 removeFromStorage 的 pending 分支）。
	c.pendingMu.Lock()
	_, pending := c.pendingDeletes["k"]
	c.pendingMu.Unlock()
	assert.False(t, pending, "降级的二次删不得写入 pendingDeletes")

	// 广播仍发生：首删一次 + 二删一次（其他实例需各自清 L1）。
	assert.GreaterOrEqual(t, lis.count("k"), 2, "二次删即便跳过 L2 也应广播 noticeRemoved")

	// 恢复后 flush 不应补删该 key。
	c.degraded.Store(false)
	c.flushPending(true)
	time.Sleep(10 * time.Millisecond)
	assert.True(t, delayRemoteHas(remote, "k"), "flush 不得因二次删补删该 key（未入队）")
}

// 同 key 重复 Invalidate：仅保留一个 timer 条目，scheduledAt（现仅作 bookkeeping）推进到最新。
func TestDelayedSecondDeleteReplacesTimer(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 50*time.Millisecond, remote) // delay 足够长，检查期间不触发
	ctx := context.Background()

	assert.NoError(t, c.Invalidate(ctx, mutateKeys("k")))
	c.delayedMu.Lock()
	first := c.delayedTimers["k"]
	assert.NotNil(t, first)
	s1 := first.scheduledAt
	c.delayedMu.Unlock()

	time.Sleep(5 * time.Millisecond)
	assert.NoError(t, c.Invalidate(ctx, mutateKeys("k")))

	c.delayedMu.Lock()
	count := len(c.delayedTimers)
	second := c.delayedTimers["k"]
	s2 := second.scheduledAt
	c.delayedMu.Unlock()

	assert.Equal(t, 1, count, "同 key 多次失效仅应保留一个 timer 条目")
	assert.NotNil(t, second)
	assert.Greater(t, s2, s1, "调度时刻应推进到最新失效时刻（旧 timer 已被 Stop）")
}

// 多 key 各自调度二次删：返回含重复项与空串的 key 集，去重后每个 key 独立调度、独立触发。
func TestDelayedSecondDeleteSchedulesPerKey(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 40*time.Millisecond, remote)
	ctx := context.Background()

	// 3 个有效 key + 重复项 a + 空串：去重后应恰好调度 a/b/c 三个。
	assert.NoError(t, c.Invalidate(ctx, mutateKeys("a", "b", "c", "a", "")))

	c.delayedMu.Lock()
	n := len(c.delayedTimers)
	_, hasA := c.delayedTimers["a"]
	_, hasB := c.delayedTimers["b"]
	_, hasC := c.delayedTimers["c"]
	_, hasEmpty := c.delayedTimers[""]
	c.delayedMu.Unlock()

	assert.Equal(t, 3, n, "去重后应恰好调度 3 个 key（重复项/空串不计）")
	assert.True(t, hasA && hasB && hasC, "a/b/c 均应各自建 timer")
	assert.False(t, hasEmpty, "空串不应建 timer")

	// 竞态回填：三个 key 各落入 L2 旧值。
	for _, k := range []string{"a", "b", "c"} {
		assert.NoError(t, remote.Put(ctx, k, makeVersionedData(100, []byte("stale")), 60))
	}

	// 短 delay 下三者的二次删各自触发、全部清除。
	ok := delayEventually(time.Second, 5*time.Millisecond, func() bool {
		return !delayRemoteHas(remote, "a") && !delayRemoteHas(remote, "b") && !delayRemoteHas(remote, "c")
	})
	assert.True(t, ok, "每个 key 的延时无条件补删都应各自触发并清除")
}

// Close 后 pending timer 不再触发（delay 取 120ms，留足 Stop 生效余量）。
func TestDelayedSecondDeleteStoppedOnClose(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 120*time.Millisecond, remote)
	ctx := context.Background()

	assert.NoError(t, c.Invalidate(ctx, mutateKeys("k")))
	// 调度后向 L1/L2 写入脏值；若 timer 触发则会被无条件清除。
	assert.NoError(t, remote.Put(ctx, "k", makeVersionedData(100, []byte("stale")), 60))
	assert.NoError(t, c.localStore.Put(ctx, "k", makeVersionedData(100, []byte("stale")), 60))

	c.Close()

	c.delayedMu.Lock()
	cleared := c.delayedTimers == nil
	c.delayedMu.Unlock()
	assert.True(t, cleared, "Close 应清空 delayedTimers")

	// 等待越过原 delay：timer 已被 Stop、或回调因 closed 直接返回，L1/L2 脏值均应保留。
	time.Sleep(140 * time.Millisecond)
	assert.True(t, delayRemoteHas(remote, "k"), "Close 后 pending timer 不得再触发远程删除")
	assert.True(t, localHas(c.localStore, "k"), "Close 后 pending timer 不得再触发本地删除")
}
