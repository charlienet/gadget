package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// dedupeKeys：去重 + 过滤空串，保持首次出现顺序。
func TestDedupeKeys(t *testing.T) {
	assert.Nil(t, dedupeKeys(nil))
	assert.Empty(t, dedupeKeys([]string{"", ""}))
	assert.Equal(t, []string{"a", "b", "c"}, dedupeKeys([]string{"a", "b", "c", "a", "", "b"}))
}

// 多 key 失效：mutate 返回含重复项与空串的集合，去重后逐个从 L1/L2 删除并各自广播一次。
func TestInvalidateMultipleKeys(t *testing.T) {
	remote := newTestRemoteStore()
	lis := &delayRecordingListener{}
	c := newDelayCache(t, 0, remote, WithListener(lis)) // 关闭二次删，聚焦批量失效本身
	ctx := context.Background()

	for _, k := range []string{"k1", "k2", "k3"} {
		assert.NoError(t, c.Put(ctx, k, "v", 60))
		assert.True(t, localHas(c.localStore, k))
		assert.True(t, delayRemoteHas(remote, k))
	}
	assert.NoError(t, c.Put(ctx, "keep", "v", 60)) // 不在返回集合中的干扰 key

	assert.NoError(t, c.Invalidate(ctx, mutateKeys("k1", "k2", "k3", "k1", "")))

	for _, k := range []string{"k1", "k2", "k3"} {
		assert.False(t, localHas(c.localStore, k), "L1 应删除 "+k)
		assert.False(t, delayRemoteHas(remote, k), "L2 应删除 "+k)
		// 去重生效：即便 mutate 返回了重复的 k1，也只删一次、广播一次。
		assert.Equal(t, 1, lis.count(k), "每个去重后的 key 应恰好广播一次")
	}
	assert.True(t, delayRemoteHas(remote, "keep"), "未列入的 key 不应被删")
	assert.Equal(t, 0, lis.count("keep"), "未列入的 key 不应被广播")
}

// (nil, nil)：合法且不影响任何 key，缓存保持不动。
func TestInvalidateNilKeysIsNoop(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 0, remote)
	ctx := context.Background()
	assert.NoError(t, c.Put(ctx, "k", "v", 60))

	assert.NoError(t, c.Invalidate(ctx, func(_ context.Context) ([]string, error) {
		return nil, nil
	}))
	assert.True(t, localHas(c.localStore, "k"))
	assert.True(t, delayRemoteHas(remote, "k"))
}

// mutateFn 返回 error：原样透传，即便同时返回了 keys 也不删除任何缓存。
func TestInvalidateMutateErrorPropagates(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 0, remote)
	ctx := context.Background()
	assert.NoError(t, c.Put(ctx, "k", "v", 60))

	sentinel := errors.New("boom")
	err := c.Invalidate(ctx, func(_ context.Context) ([]string, error) {
		return []string{"k"}, sentinel
	})
	assert.ErrorIs(t, err, sentinel)
	assert.True(t, localHas(c.localStore, "k"), "源未变更不应删除 L1")
	assert.True(t, delayRemoteHas(remote, "k"), "源未变更不应删除 L2")
}

// 并发交叉 key 集（A:[k1,k2] vs B:[k2,k1]）：逐 key 单锁、Do 后立即 Forget，无交叉持锁，
// 带超时兜底断言不死锁（配合 -race 验证无数据竞争）。用例耗时毫秒级。
func TestInvalidateConcurrentCrossKeysNoDeadlock(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 0, remote)
	ctx := context.Background()
	for _, k := range []string{"k1", "k2"} {
		assert.NoError(t, c.Put(ctx, k, "v", 60))
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	inv := func(keys ...string) {
		defer wg.Done()
		<-start
		_ = c.Invalidate(ctx, mutateKeys(keys...))
	}
	wg.Add(2)
	go inv("k1", "k2")
	go inv("k2", "k1")
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: cross key sets (k1,k2) vs (k2,k1) must not deadlock")
	}

	assert.False(t, delayRemoteHas(remote, "k1"))
	assert.False(t, delayRemoteHas(remote, "k2"))
}

// TestInvalidateNotSwallowedByConcurrentGetfn 以 channel 确定性地构造 B1 竞态：
// Getfn(k) 的 loadFn 读到 mutate 前旧值并阻塞（key:k 的 singleflight 处于 in-flight），
// 此时发起的 Invalidate(k) 其删除闭包会落入同一 in-flight；未修复 B1 时 singleflight
// 共享语义使 Invalidate 的闭包整体被吞（Delete + 二次删调度都不执行），旧值残留 L1/L2。
// B1 修复（shared 分支补删）后，Delete 确定性晚于回填，旧值被清除。关闭二次删以隔离断言。
func TestInvalidateNotSwallowedByConcurrentGetfn(t *testing.T) {
	remote := newTestRemoteStore()
	c := newDelayCache(t, 0, remote) // 关闭延时二次删：只验证"首删是否被共享吞掉"
	ctx := context.Background()

	// L1/L2 起始为空，确保 Getfn 必走 loadFn（回源），loadFn 模拟读到 mutate 前旧值。
	inFlight := make(chan struct{})
	release := make(chan struct{})

	getfnErr := make(chan error, 1)
	go func() {
		var s string
		getfnErr <- c.Getfn(ctx, "k", &s, func(_ context.Context, _ string, v any) (bool, error) {
			if pv, ok := v.(*string); ok {
				*pv = "old" // 脏值：mutate 前的旧数据
			}
			close(inFlight) // 通知：已进入 loadFn，key:k 的 singleflight 已注册 in-flight
			<-release       // 阻塞，把"回填落地"推迟到 Invalidate 已抵达等待之后
			return true, nil
		}, 60)
	}()

	<-inFlight // Getfn 的 Do 已建立 in-flight 条目

	invErr := make(chan error, 1)
	go func() {
		invErr <- c.Invalidate(ctx, mutateKeys("k")) // 删除闭包命中 Getfn 的 in-flight
	}()
	time.Sleep(30 * time.Millisecond) // 让 Invalidate 抵达 sg.Do 并注册为等待者

	close(release) // 放行 Getfn：回填旧值 → Do 完成 → 唤醒共享的 Invalidate.Do

	assert.NoError(t, <-getfnErr)
	assert.NoError(t, <-invErr)

	// ★ 终态：修复后 shared 分支补删必已执行；未修复则 Delete 被共享吞、旧值残留 L1/L2。
	assert.False(t, localHas(c.localStore, "k"), "L1 旧值应被 Invalidate 删除（B1 未修复时残留 → 红）")
	assert.False(t, delayRemoteHas(remote, "k"), "L2 旧值应被 Invalidate 删除（B1 未修复时残留 → 红）")
}
