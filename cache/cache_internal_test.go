package cache

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// makeVersionedData creates version-wrapped data with an explicit timestamp.
func makeVersionedData(ts int64, payload []byte) []byte {
	buf := make([]byte, versionPrefixLen+len(payload))
	buf[0] = versionMarker
	binary.BigEndian.PutUint64(buf[1:versionPrefixLen], uint64(ts))
	copy(buf[versionPrefixLen:], payload)
	return buf
}

// testRemoteStore is a simple remote store implementation for internal tests.
type testRemoteStore struct {
	data map[string][]byte
	mu   sync.Mutex
}

func newTestRemoteStore() *testRemoteStore {
	return &testRemoteStore{data: make(map[string][]byte)}
}

func (s *testRemoteStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	if !ok {
		return nil, false, nil
	}
	return v, true, nil
}

func (s *testRemoteStore) Put(_ context.Context, key string, v []byte, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = v
	return nil
}

func (s *testRemoteStore) Delete(_ context.Context, keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		delete(s.data, k)
	}
	return nil
}

func (s *testRemoteStore) Name() string   { return "test-remote" }
func (s *testRemoteStore) IsRemote() bool { return true }

// --- shouldVerify / resetVerifyCount ---

func TestShouldVerify(t *testing.T) {
	c := &cache{verifyEvery: 3, stopChan: make(chan struct{})}

	// First 2 calls: no verify
	assert.False(t, c.shouldVerify("k"))
	assert.False(t, c.shouldVerify("k"))

	// 3rd call: verify = true, counter resets
	assert.True(t, c.shouldVerify("k"))

	// Next 3 calls loop again
	assert.False(t, c.shouldVerify("k"))
	assert.False(t, c.shouldVerify("k"))
	assert.True(t, c.shouldVerify("k"))
}

func TestShouldVerifyDisabled(t *testing.T) {
	c := &cache{verifyEvery: 0, stopChan: make(chan struct{})}
	assert.False(t, c.shouldVerify("any-key"))
}

func TestResetVerifyCount(t *testing.T) {
	c := &cache{verifyEvery: 3, stopChan: make(chan struct{})}

	c.shouldVerify("k1")
	c.shouldVerify("k1")
	c.resetVerifyCount("k1")
	// After reset, counter should start from 0
	assert.False(t, c.shouldVerify("k1"))
	assert.False(t, c.shouldVerify("k1"))
	assert.True(t, c.shouldVerify("k1")) // 3rd access after reset
}

func TestResetVerifyCountDisabled(t *testing.T) {
	// Should not panic when verifyEvery is 0
	c := &cache{verifyEvery: 0, stopChan: make(chan struct{})}
	c.resetVerifyCount("k1") // no-op, must not panic
}

// --- syncBatch ---

func TestSyncBatchUpdatesStaleLocal(t *testing.T) {
	local := newMemStore()
	remote := newTestRemoteStore()

	ctx := context.Background()
	_ = local.Put(ctx, "key", makeVersionedData(100, []byte("old")), 60)
	_ = remote.Put(ctx, "key", makeVersionedData(200, []byte("new")), 60)

	c := &cache{
		localStore:       local,
		remoteStore:      remote,
		ttl:              60,
		versionSyncBatch: defaultVersionSyncBatch,
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}

	c.syncBatch()

	data, exist, _ := local.Get(ctx, "key")
	assert.True(t, exist)
	assert.Equal(t, []byte("new"), payloadOf(data))
}

func TestSyncBatchEvictsStaleLocalWhenRemoteGone(t *testing.T) {
	local := newMemStore()
	remote := newTestRemoteStore()

	ctx := context.Background()
	_ = local.Put(ctx, "gone", []byte("stale"), 60)
	// remote does NOT have "gone"

	c := &cache{
		localStore:       local,
		remoteStore:      remote,
		versionSyncBatch: defaultVersionSyncBatch,
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}

	c.syncBatch()

	_, exist, _ := local.Get(ctx, "gone")
	assert.False(t, exist)
}

func TestSyncBatchSkipsWhenDegraded(t *testing.T) {
	local := newMemStore()
	remote := newTestRemoteStore()

	_ = local.Put(context.Background(), "k", makeVersionedData(100, []byte("v")), 60)
	_ = remote.Put(context.Background(), "k", makeVersionedData(200, []byte("changed")), 60)

	c := &cache{
		localStore:  local,
		remoteStore: remote,
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)

	c.syncBatch()

	data, exist, _ := local.Get(context.Background(), "k")
	assert.True(t, exist)
	assert.Equal(t, []byte("v"), payloadOf(data))
}

func TestSyncBatchNoLocal(t *testing.T) {
	c := &cache{
		localStore:  nil,
		remoteStore: newTestRemoteStore(),
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	// syncBatch checks localStore.(*mem_store); nil should not panic
	c.syncBatch()
}

func TestSyncBatchEmptyStore(t *testing.T) {
	local := newMemStore()
	remote := newTestRemoteStore()

	c := &cache{
		localStore:       local,
		remoteStore:      remote,
		versionSyncBatch: defaultVersionSyncBatch,
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}

	// Empty store → cursor reset to 0, no error
	c.syncBatch()
	assert.Equal(t, 0, c.versionCursor)

	// Put some items, sync once
	_ = local.Put(context.Background(), "a", []byte("1"), 60)
	_ = local.Put(context.Background(), "b", []byte("2"), 60)
	c.syncBatch()
	// After syncing, cursor moves forward
	assert.Greater(t, c.versionCursor, 0)

	// Sync again to trigger cursor wrap
	c.versionCursor = 100 // force wrap
	c.syncBatch()
	assert.LessOrEqual(t, c.versionCursor, local.Len())
}

// --- versionSyncLoop ---

func TestVersionSyncLoop(t *testing.T) {
	local := newMemStore()
	remote := newTestRemoteStore()

	ctx := context.Background()
	_ = local.Put(ctx, "key", makeVersionedData(100, []byte("old")), 60)
	_ = remote.Put(ctx, "key", makeVersionedData(200, []byte("new")), 60)

	c := &cache{
		localStore:          local,
		remoteStore:         remote,
		ttl:                 60,
		logger:              slog.Default(),
		versionSyncInterval: 10 * time.Millisecond,
		versionSyncBatch:    100,
		versionStop:         make(chan struct{}),
		stopChan:            make(chan struct{}),
	}

	go c.versionSyncLoop()

	// Wait for at least one sync cycle
	time.Sleep(50 * time.Millisecond)
	close(c.versionStop)

	// Local should be updated by sync
	data, exist, _ := local.Get(ctx, "key")
	assert.True(t, exist)
	assert.Equal(t, []byte("new"), payloadOf(data))
}

func TestVersionSyncLoopStopViaStopChan(t *testing.T) {
	c := &cache{
		versionSyncInterval: 10 * time.Millisecond,
		versionStop:         make(chan struct{}),
		stopChan:            make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		c.versionSyncLoop()
		close(done)
	}()

	// Close via stopChan (the other channel)
	close(c.stopChan)

	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		t.Fatal("versionSyncLoop did not exit after stopChan closed")
	}
}

// --- SampleKeys and Len ---

func TestSampleKeysAndLen(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()

	// Should return 0 for empty store
	assert.Equal(t, 0, s.Len())
	assert.Nil(t, s.SampleKeys(0, 10))

	_ = s.Put(ctx, "a", []byte("1"), 60)
	_ = s.Put(ctx, "b", []byte("2"), 60)
	_ = s.Put(ctx, "c", []byte("3"), 60)

	assert.Equal(t, 3, s.Len())

	keys := s.SampleKeys(0, 2)
	assert.Equal(t, 2, len(keys))
	assert.Equal(t, "a", keys[0])
	assert.Equal(t, "b", keys[1])

	keys = s.SampleKeys(2, 10)
	assert.Equal(t, 1, len(keys))
	assert.Equal(t, "c", keys[0])

	// Offset past end
	keys = s.SampleKeys(10, 5)
	assert.Nil(t, keys)
}

// --- MemStore DeletePattern ---

func TestMemStoreDeletePattern(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()

	_ = s.Put(ctx, "user:1", []byte("a"), 60)
	_ = s.Put(ctx, "user:2", []byte("b"), 60)
	_ = s.Put(ctx, "admin:1", []byte("c"), 60)

	_ = s.DeletePattern(ctx, "user:*")

	assert.Equal(t, 1, s.Len())
	_, exist, _ := s.Get(ctx, "admin:1")
	assert.True(t, exist)

	// Deleted keys should be gone
	_, exist, _ = s.Get(ctx, "user:1")
	assert.False(t, exist)
	_, exist, _ = s.Get(ctx, "user:2")
	assert.False(t, exist)
}

func TestMemStoreDeletePatternInvalidPattern(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()

	_ = s.Put(ctx, "a", []byte("1"), 60)
	// An invalid glob pattern should be silently skipped (continue)
	err := s.DeletePattern(ctx, "[invalid")
	assert.Nil(t, err)
	// Key should still exist
	_, exist, _ := s.Get(ctx, "a")
	assert.True(t, exist)
}

// --- Cache-level DeletePattern ---

func TestCacheDeletePattern(t *testing.T) {
	local := newMemStore()
	c := &cache{
		localStore: local,
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	ctx := context.Background()

	_ = local.Put(ctx, "x:1", []byte("v1"), 60)
	_ = local.Put(ctx, "x:2", []byte("v2"), 60)
	_ = local.Put(ctx, "y:1", []byte("v3"), 60)

	c.DeletePattern(ctx, "x:*")

	_, exist, _ := local.Get(ctx, "x:1")
	assert.False(t, exist)
	_, exist, _ = local.Get(ctx, "x:2")
	assert.False(t, exist)
	_, exist, _ = local.Get(ctx, "y:1")
	assert.True(t, exist)
}

func TestCacheDeletePatternNoRemote(t *testing.T) {
	// Should not panic when remoteStore is nil
	c := &cache{
		localStore: newMemStore(),
		stopChan:   make(chan struct{}),
	}
	ctx := context.Background()
	_ = c.localStore.Put(ctx, "k", []byte("v"), 60)
	c.DeletePattern(ctx, "k")
	_, exist, _ := c.localStore.Get(ctx, "k")
	assert.False(t, exist)
}

// --- recordRemoteError / recordRemoteSuccess ---

func TestRecordRemoteErrorTriggersDegraded(t *testing.T) {
	c := &cache{
		degradeThreshold: 3,
		degradeRecovery:  100 * time.Millisecond,
		metrics:          noopMetrics{},
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}

	assert.False(t, c.isDegraded())

	c.recordRemoteError(assert.AnError)
	assert.False(t, c.isDegraded())

	c.recordRemoteError(assert.AnError)
	assert.False(t, c.isDegraded())

	c.recordRemoteError(assert.AnError)
	assert.True(t, c.isDegraded())
}

func TestRecordRemoteErrorNoThreshold(t *testing.T) {
	// degradeThreshold = 0 means no degrade
	c := &cache{
		degradeThreshold: 0,
		metrics:          noopMetrics{},
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}

	for i := 0; i < 10; i++ {
		c.recordRemoteError(assert.AnError)
	}
	assert.False(t, c.isDegraded())
}

func TestRecordRemoteSuccessExitsDegraded(t *testing.T) {
	c := &cache{
		degradeThreshold: 1,
		metrics:          noopMetrics{},
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}
	c.degraded.Store(true)
	assert.True(t, c.isDegraded())

	c.recordRemoteSuccess()
	assert.False(t, c.isDegraded())
}

// --- healthLoop ---

func TestHealthLoopStopsViaDegradeStopRecov(t *testing.T) {
	remote := newTestRemoteStore()
	c := &cache{
		remoteStore:      remote,
		degradeRecovery:  10 * time.Millisecond,
		degradeStopRecov: make(chan struct{}),
		stopChan:         make(chan struct{}),
		metrics:          noopMetrics{},
		logger:           slog.Default(),
	}

	done := make(chan struct{})
	go func() {
		c.healthLoop()
		close(done)
	}()

	// Give it a tick or two
	time.Sleep(30 * time.Millisecond)
	close(c.degradeStopRecov)

	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		t.Fatal("healthLoop did not exit after degradeStopRecov closed")
	}
}

func TestHealthLoopStopsViaStopChan(t *testing.T) {
	remote := newTestRemoteStore()
	c := &cache{
		remoteStore:      remote,
		degradeRecovery:  10 * time.Millisecond,
		degradeStopRecov: make(chan struct{}),
		stopChan:         make(chan struct{}),
		metrics:          noopMetrics{},
		logger:           slog.Default(),
	}

	done := make(chan struct{})
	go func() {
		c.healthLoop()
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	close(c.stopChan)

	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		t.Fatal("healthLoop did not exit after stopChan closed")
	}
}

// --- getFromStore degraded skip ---

func TestGetFromStoreSkipsRemoteWhenDegraded(t *testing.T) {
	remote := newTestRemoteStore()
	_ = remote.Put(context.Background(), "k", []byte("v"), 60)

	c := &cache{
		remoteStore: remote,
		stats:       newStats(),
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)

	data, exist, err := c.getFromStore(context.Background(), remote, "k")
	assert.Nil(t, err)
	assert.False(t, exist)
	assert.Equal(t, []byte{}, data)
}

func TestGetFromStoreRecordsRemoteError(t *testing.T) {
	// errorStore implementation that always fails
	failRemote := &failStore{}

	c := &cache{
		remoteStore:      failRemote,
		degradeThreshold: 2,
		metrics:          noopMetrics{},
		stats:            newStats(),
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}

	_, _, err := c.getFromStore(context.Background(), failRemote, "k")
	assert.Error(t, err)
	assert.Equal(t, int64(1), c.degradeCount.Load())

	_, _, err = c.getFromStore(context.Background(), failRemote, "k")
	assert.Error(t, err)
	assert.True(t, c.isDegraded())
}

// failStore always returns an error on Get
type failStore struct{}

func (f *failStore) Get(_ context.Context, _ string) ([]byte, bool, error) {
	return nil, false, assert.AnError
}
func (f *failStore) Put(_ context.Context, _ string, _ []byte, _ int) error {
	return nil
}
func (f *failStore) Delete(_ context.Context, _ ...string) error {
	return nil
}
func (f *failStore) Name() string   { return "fail" }
func (f *failStore) IsRemote() bool { return true }

// --- noopMetrics usage from mem_store ---

func TestMemStoreEvictionCallsNoopMetrics(t *testing.T) {
	// noopMetrics.CacheEviction is called during eviction; should not panic
	s := newMemStore()
	s.maxItems = 2
	ctx := context.Background()

	_ = s.Put(ctx, "a", []byte("1"), 60)
	_ = s.Put(ctx, "b", []byte("2"), 60)
	_ = s.Put(ctx, "c", []byte("3"), 60) // triggers eviction → calls noopMetrics.CacheEviction()

	assert.Equal(t, 2, s.Len())
}

// --- putInStore and removeFromStorage with nil store ---

func TestPutInStoreNil(t *testing.T) {
	c := &cache{stopChan: make(chan struct{})}
	err := c.putInStore(context.Background(), nil, "k", []byte("v"), 60)
	assert.Nil(t, err)
}

func TestRemoveFromStorageNil(t *testing.T) {
	c := &cache{stopChan: make(chan struct{})}
	// Should not panic
	c.removeFromStorage(context.Background(), nil, "k")
}

func TestRemoveFromStorageDegradedRemote(t *testing.T) {
	remote := newTestRemoteStore()
	_ = remote.Put(context.Background(), "k", []byte("v"), 60)

	c := &cache{stopChan: make(chan struct{})}
	c.degraded.Store(true)

	// Should skip deletion on degraded remote
	c.removeFromStorage(context.Background(), remote, "k")
	_, exist, _ := remote.Get(context.Background(), "k")
	assert.True(t, exist, "remote should still have the key since degraded skips deletion")
}

// --- getFromStore with nil store ---

func TestGetFromStoreNil(t *testing.T) {
	c := &cache{stopChan: make(chan struct{})}
	data, exist, err := c.getFromStore(context.Background(), nil, "k")
	assert.Nil(t, err)
	assert.False(t, exist)
	assert.Equal(t, []byte{}, data)
}

// --- H1: 降级期间 pending 写入，恢复后 flush 补偿写 ---

func TestFlushPendingWritesOnRecovery(t *testing.T) {
	remote := newTestRemoteStore()
	local := newMemStore()
	c := &cache{
		localStore:  local,
		remoteStore: remote,
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)
	ctx := context.Background()

	// 降级期间写入：本地成功，remote 写入进入 pending 缓冲
	err := c.putCache(ctx, "k", []byte("v"), 60)
	assert.Nil(t, err)
	assert.Len(t, c.pendingWrites, 1)

	_, exist, _ := local.Get(ctx, "k")
	assert.True(t, exist, "local write should succeed while degraded")
	_, exist, _ = remote.Get(ctx, "k")
	assert.False(t, exist, "remote must not receive writes while degraded")

	// 退出降级 → flush pending → remote 端补偿写成功
	c.recordRemoteSuccess()
	assert.False(t, c.isDegraded())
	assert.Empty(t, c.pendingWrites)

	data, exist, _ := remote.Get(ctx, "k")
	assert.True(t, exist, "remote should receive buffered write after recovery")
	assert.Equal(t, []byte("v"), payloadOf(data))
}

func TestFlushPendingWritesFailureKeepsForRetry(t *testing.T) {
	remote := &putFailStore{testRemoteStore: newTestRemoteStore()}
	local := newMemStore()
	c := &cache{
		localStore:       local,
		remoteStore:      remote,
		versionSyncBatch: defaultVersionSyncBatch,
		metrics:          noopMetrics{},
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}
	c.degraded.Store(true)
	ctx := context.Background()

	_ = c.putCache(ctx, "k", []byte("v"), 60)
	assert.Len(t, c.pendingWrites, 1)

	// flush 失败：保留在 pending 中等待重试（不丢弃）
	c.recordRemoteSuccess()
	assert.Len(t, c.pendingWrites, 1, "failed flush should keep the pending write for retry")

	// syncBatch 不得驱逐等待补偿写的 key
	c.syncBatch()
	assert.Len(t, c.pendingWrites, 1, "pending write must survive version sync")
	_, localExist, _ := local.Get(ctx, "k")
	assert.True(t, localExist, "pending key must not be evicted by version sync")

	// remote 恢复后（换为可写 store）再次触发 flush → 补偿写成功。
	// 非转换点触发受 flushRetryInterval 退避控制，测试中重置时间戳允许立即重试。
	c.remoteStore = newTestRemoteStore()
	c.pendingMu.Lock()
	c.lastFlush = time.Time{}
	c.pendingMu.Unlock()
	c.recordRemoteSuccess()
	assert.Empty(t, c.pendingWrites, "retry should succeed and clear pending")

	data, exist, _ := c.remoteStore.Get(ctx, "k")
	assert.True(t, exist)
	assert.Equal(t, []byte("v"), payloadOf(data))
}

// putFailStore 的 Put 恒失败，用于验证 flush 失败丢弃策略。
type putFailStore struct {
	*testRemoteStore
}

func (s *putFailStore) Put(_ context.Context, _ string, _ []byte, _ int) error {
	return assert.AnError
}

// --- H2: 概率验证路径 remote 出错时保留本地数据 ---

func TestVerifyRemoteErrorKeepsLocal(t *testing.T) {
	local := newMemStore()
	remote := &failStore{} // Get 恒失败

	c := &cache{
		localStore:  local,
		remoteStore: remote,
		verifyEvery: 1,
		metrics:     noopMetrics{},
		stats:       newStats(),
		logger:      slog.Default(),
		ttl:         60,
		stopChan:    make(chan struct{}),
	}
	ctx := context.Background()
	_ = local.Put(ctx, "k", makeVersionedData(100, []byte(`"local"`)), 60)

	// verify 路径 remote 出错：不返回错误、保留并返回本地数据
	data, exist, err := c.getFromCache(ctx, "k", 0)
	assert.Nil(t, err)
	assert.True(t, exist)
	assert.Equal(t, []byte(`"local"`), data)

	// 本地数据未被清除
	_, localExist, _ := local.Get(ctx, "k")
	assert.True(t, localExist, "local data must not be evicted on remote error")

	// 错误已计入降级计数（failStore 为 remote 类型）
	assert.GreaterOrEqual(t, c.degradeCount.Load(), int64(1))
}

func TestVerifyRemoteErrorNoLocalPropagates(t *testing.T) {
	local := newMemStore()
	remote := &failStore{} // Get 恒失败

	c := &cache{
		localStore:  local,
		remoteStore: remote,
		verifyEvery: 1,
		metrics:     noopMetrics{},
		stats:       newStats(),
		logger:      slog.Default(),
		ttl:         60,
		stopChan:    make(chan struct{}),
	}
	ctx := context.Background()

	// 本地无数据时 remote 出错 → 错误向上传播
	data, exist, err := c.getFromCache(ctx, "k", 0)
	assert.Error(t, err)
	assert.False(t, exist)
	assert.Nil(t, data)
}

// --- M2: 写路径驱动降级（putInStore / removeFromStorage remote 失败） ---

func TestPutInStoreRemoteErrorTriggersDegraded(t *testing.T) {
	local := newMemStore()
	remote := &putFailStore{testRemoteStore: newTestRemoteStore()}
	c := &cache{
		localStore:       local,
		remoteStore:      remote,
		degradeThreshold: 2,
		metrics:          noopMetrics{},
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}
	ctx := context.Background()

	err := c.putCache(ctx, "k", []byte("v"), 60)
	assert.Error(t, err, "remote Put failure should propagate")
	assert.False(t, c.isDegraded())

	err = c.putCache(ctx, "k2", []byte("v"), 60)
	assert.Error(t, err)
	assert.True(t, c.isDegraded(), "write-path remote failures should drive degraded mode")
}

func TestRemoveFromStorageRemoteErrorTriggersDegraded(t *testing.T) {
	local := newMemStore()
	remote := &deleteFailStore{testRemoteStore: newTestRemoteStore()}
	c := &cache{
		localStore:       local,
		remoteStore:      remote,
		degradeThreshold: 2,
		metrics:          noopMetrics{},
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}
	ctx := context.Background()

	c.removeFromStorage(ctx, remote, "k")
	assert.False(t, c.isDegraded())

	c.removeFromStorage(ctx, remote, "k")
	assert.True(t, c.isDegraded(), "delete-path remote failures should drive degraded mode")
}

// deleteFailStore 的 Delete 恒失败，用于验证 removeFromStorage 的错误路径。
type deleteFailStore struct {
	*testRemoteStore
}

func (s *deleteFailStore) Delete(_ context.Context, _ ...string) error {
	return assert.AnError
}

// --- M5: verify 回写 L1 使用请求路径 TTL ---

func TestGetfnVerifyWriteBackUsesRequestTTL(t *testing.T) {
	local := newMemStore()
	remote := newTestRemoteStore()
	c := &cache{
		localStore:  local,
		remoteStore: remote,
		verifyEvery: 1,
		stats:       newStats(),
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		ttl:         60, // 固定 TTL，验证回写应被请求 TTL 覆盖
		stopChan:    make(chan struct{}),
	}
	ctx := context.Background()

	_ = local.Put(ctx, "k", makeVersionedData(100, []byte("old")), 60)
	_ = remote.Put(ctx, "k", makeVersionedData(200, []byte("new")), 60)

	// Getfn 路径传入 expireSeconds=100：verify 回写 local 应使用 100s 而非 c.ttl=60s
	_, exist, err := c.getFromCache(ctx, "k", 100)
	assert.Nil(t, err)
	assert.True(t, exist)

	local.RLock()
	item, ok := local.items["k"]
	local.RUnlock()
	assert.True(t, ok)
	remaining := item.Expiration - time.Now().UnixNano()
	assert.InDelta(t, float64(100*time.Second), float64(remaining), float64(5*time.Second),
		"L1 write-back TTL should use request-path expireSeconds")
}

// --- M7: removeFromStorage Delete 错误记录 Warn ---

// mockLogger 记录 Warn/Error 级别日志（消息文本 + 结构化属性），用于断言告警路径。
// 实现 slog.Handler，经 slog.New(ml) 注入 cache.logger 字段。
type mockLogger struct {
	mu        sync.Mutex
	warns     []string
	warnAttrs []map[string]any
	errs      []string
}

func (l *mockLogger) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (l *mockLogger) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case r.Level >= slog.LevelError:
		l.errs = append(l.errs, r.Message)
	case r.Level >= slog.LevelWarn:
		attrs := make(map[string]any, r.NumAttrs())
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.Any()
			return true
		})
		l.warns = append(l.warns, r.Message)
		l.warnAttrs = append(l.warnAttrs, attrs)
	}
	return nil
}

// attrsOf 返回首条消息包含 substr 的 Warn 记录的结构化属性；无匹配返回 nil。
func (l *mockLogger) attrsOf(substr string) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, m := range l.warns {
		if strings.Contains(m, substr) {
			return l.warnAttrs[i]
		}
	}
	return nil
}

func (l *mockLogger) WithAttrs(_ []slog.Attr) slog.Handler { return l }
func (l *mockLogger) WithGroup(_ string) slog.Handler      { return l }

func TestRemoveFromStorageDeleteErrorWarns(t *testing.T) {
	remote := &deleteFailStore{testRemoteStore: newTestRemoteStore()}
	ml := &mockLogger{}
	c := &cache{
		remoteStore: remote,
		logger:      slog.New(ml),
		stopChan:    make(chan struct{}),
	}
	ctx := context.Background()

	c.removeFromStorage(ctx, remote, "k")
	assert.Len(t, ml.warns, 1, "delete failure should be logged via Warn")
	assert.Contains(t, ml.warns[0], "delete from store")
	// 结构化属性：store/keys/err 均不得丢失
	attrs := ml.attrsOf("delete from store")
	assert.Equal(t, remote.Name(), attrs["store"])
	assert.Equal(t, []string{"k"}, attrs["keys"])
	assert.Equal(t, assert.AnError, attrs["err"])
}

// failListener 的 Publish 恒失败。
type failListener struct{}

func (f *failListener) Subscribe() chan string      { return make(chan string) }
func (f *failListener) Publish(string) error        { return assert.AnError }
func (f *failListener) Ready() <-chan struct{}      { return closedChan() }
func (f *failListener) Close(context.Context) error { return nil }

// closedChan 返回一个已关闭的 channel（表示监听器立即可用）。
func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func TestNoticeRemovedPublishErrorWarns(t *testing.T) {
	ml := &mockLogger{}
	c := &cache{
		listener: &failListener{},
		logger:   slog.New(ml),
		stopChan: make(chan struct{}),
	}
	c.noticeRemoved("k")
	assert.Len(t, ml.warns, 1, "publish failure should be logged via Warn")
	assert.Contains(t, ml.warns[0], "publish removed key")
}

// --- 降级期间 Get/Getfn 行为 ---

func TestDegradedGetReturnsNotExist(t *testing.T) {
	remote := newTestRemoteStore()
	c := &cache{
		localStore:  newMemStore(),
		remoteStore: remote,
		stats:       newStats(),
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)
	ctx := context.Background()

	var s string
	err := c.Get(ctx, "missing", &s)
	assert.ErrorIs(t, err, ErrEntityNotExist, "degraded Get on local miss should return not-exist")
}

func TestDegradedGetfnCallsLoadFn(t *testing.T) {
	remote := newTestRemoteStore()
	c := &cache{
		localStore:  newMemStore(),
		remoteStore: remote,
		serializer:  jsonSerializer{},
		stats:       newStats(),
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)
	ctx := context.Background()

	called := false
	var s string
	err := c.Getfn(ctx, "k", &s, func(ctx context.Context, key string, v any) (bool, error) {
		called = true
		if sv, ok := v.(*string); ok {
			*sv = "loaded"
		}
		return true, nil
	}, 60)
	assert.Nil(t, err)
	assert.True(t, called, "degraded Getfn should still fall back to loadFn")
	assert.Equal(t, "loaded", s)
}

// --- 序列化失败路径 ---

// failSerializer 的 Marshal 恒失败。
type failSerializer struct{}

func (failSerializer) Marshal(v any) ([]byte, error)   { return nil, assert.AnError }
func (failSerializer) Unmarshal(b []byte, v any) error { return nil }

func TestPutMarshalError(t *testing.T) {
	c := &cache{
		serializer: failSerializer{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	err := c.Put(context.Background(), "k", "v", 60)
	assert.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError, "marshal error should be wrapped with %w")
}

// unmarshalFailSerializer 的 Unmarshal 恒失败。
type unmarshalFailSerializer struct{}

func (unmarshalFailSerializer) Marshal(v any) ([]byte, error)   { return []byte("x"), nil }
func (unmarshalFailSerializer) Unmarshal(b []byte, v any) error { return assert.AnError }

func TestGetUnmarshalError(t *testing.T) {
	local := newMemStore()
	_ = local.Put(context.Background(), "k", []byte("x"), 60)
	c := &cache{
		localStore: local,
		serializer: unmarshalFailSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	var s string
	err := c.Get(context.Background(), "k", &s)
	assert.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError, "unmarshal error should be wrapped with %w")
}

func TestGetfnMarshalErrorSkipsCaching(t *testing.T) {
	ml := &mockLogger{}
	c := &cache{
		serializer:          failSerializer{},
		notExistPlaceholder: []byte(defaultNotExistPlaceholder),
		stats:               newStats(),
		metrics:             noopMetrics{},
		logger:              slog.New(ml),
		stopChan:            make(chan struct{}),
	}
	ctx := context.Background()

	var s string
	err := c.Getfn(ctx, "k", &s, func(ctx context.Context, key string, v any) (bool, error) {
		if sv, ok := v.(*string); ok {
			*sv = "val"
		}
		return true, nil
	}, 60)
	assert.Nil(t, err, "marshal failure must not block the source result")
	assert.Contains(t, ml.warns[0], "marshal loaded value")

	// 未缓存 → 再次 Getfn 仍会调用 loadFn
	called := false
	_ = c.Getfn(ctx, "k", &s, func(ctx context.Context, key string, v any) (bool, error) {
		called = true
		return true, nil
	}, 60)
	assert.True(t, called, "failed marshal should skip cache write")
}

// --- verify 路径：remote 确认无数据时清除 stale local ---

func TestVerifyRemoteGoneClearsStaleLocal(t *testing.T) {
	local := newMemStore()
	remote := newTestRemoteStore()
	c := &cache{
		localStore:  local,
		remoteStore: remote,
		verifyEvery: 1,
		stats:       newStats(),
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		ttl:         60,
		stopChan:    make(chan struct{}),
	}
	ctx := context.Background()
	_ = local.Put(ctx, "k", makeVersionedData(100, []byte("old")), 60)
	// remote 无该 key

	_, exist, err := c.getFromCache(ctx, "k", 0)
	assert.Nil(t, err)
	assert.False(t, exist)

	_, localExist, _ := local.Get(ctx, "k")
	assert.False(t, localExist, "stale local entry should be cleared when remote confirms absence")
}

// --- 基础组件覆盖：logger / options / stats / mem_store 补全 ---

func TestDefaultLoggerMethods(t *testing.T) {
	// DefaultLogger 的 Warn/Error 应可安全调用（输出到 stderr 的 Warn 及以上）
	l := slog.Default()
	assert.NotPanics(t, func() {
		l.Warn("warn")
		l.Warn(fmt.Sprintf("warn %d", 1))
		l.Error("err")
		l.Error(fmt.Sprintf("err %d", 1))
	})
}

func TestOptionFunctionsWithValues(t *testing.T) {
	var o Options
	WithListener(&failListener{})(&o)
	assert.NotNil(t, o.listener)

	WithStore(newTestRemoteStore())(&o)
	assert.Equal(t, o.remoteStore, o.remoteStore)

	WithSerializer(failSerializer{})(&o)
	assert.NotNil(t, o.serializer)

	WithLogger(slog.Default())(&o)
	assert.NotNil(t, o.Logger)

	WithTTLJitter(5 * time.Millisecond)(&o)
	assert.Equal(t, 5*time.Millisecond, o.ttlJitter)

	WithSlidingWindow(10 * time.Second)(&o)
	assert.Equal(t, 10*time.Second, o.slidingWindow)
}

func TestStatsClear(t *testing.T) {
	s := newStats()
	s.IncrHit("a")
	s.IncrMiss("b")
	s.IncrQuery()
	s.Clear()
	assert.Equal(t, uint64(0), s.TotalHits())
	assert.Equal(t, uint64(0), s.TotalMiss())
	assert.Equal(t, uint64(0), s.Query)
}

func TestMemStoreGetMultiDirect(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()
	_ = s.Put(ctx, "a", []byte("1"), 60)
	_ = s.Put(ctx, "b", []byte("2"), 60)

	res, err := s.GetMulti(ctx, "a", "b", "missing")
	assert.Nil(t, err)
	assert.Len(t, res, 2)
	assert.Equal(t, []byte("1"), res["a"])
	assert.Equal(t, []byte("2"), res["b"])
}

// TestSetMultiFallbackPath 覆盖非 BulkStore 的逐 key 写入分支。
func TestSetMultiFallbackPath(t *testing.T) {
	local := newMockLocalStore() // 不实现 BulkStore → 走逐 key fallback
	c := &cache{
		localStore: local,
		serializer: jsonSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		ttl:        60,
		stopChan:   make(chan struct{}),
	}
	err := c.SetMulti(context.Background(), map[string]any{"a": "1", "b": "2"}, 60)
	assert.Nil(t, err)

	var s string
	assert.Nil(t, c.Get(context.Background(), "a", &s))
	assert.Equal(t, "1", s)
}

// mockLocalStore 是最简 local store 实现，不实现 BulkStore。
type mockLocalStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMockLocalStore() *mockLocalStore { return &mockLocalStore{data: make(map[string][]byte)} }

func (s *mockLocalStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok, nil
}

func (s *mockLocalStore) Put(_ context.Context, key string, v []byte, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = v
	return nil
}

func (s *mockLocalStore) Delete(_ context.Context, keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		delete(s.data, k)
	}
	return nil
}

func (s *mockLocalStore) Name() string   { return "mock-local" }
func (s *mockLocalStore) IsRemote() bool { return false }

// --- 覆盖率补全：错误传播 / 分支覆盖 ---

// bulkFailStore 实现 BulkStore 且 SetMulti 恒失败。
type bulkFailStore struct {
	*mockLocalStore
}

func (s *bulkFailStore) GetMulti(_ context.Context, _ ...string) (map[string][]byte, error) {
	return nil, nil
}

func (s *bulkFailStore) SetMulti(_ context.Context, _ map[string][]byte, _ int) error {
	return assert.AnError
}

// closableListener 的 Close 会关闭订阅 channel（触发 watcher ok=false 退出）。
// 实现幂等：重复调用安全。
type closableListener struct {
	ch        chan string
	closeOnce sync.Once
}

func (l *closableListener) Subscribe() chan string { return l.ch }
func (l *closableListener) Publish(string) error   { return nil }
func (l *closableListener) Ready() <-chan struct{} { return closedChan() }
func (l *closableListener) Close(context.Context) error {
	l.closeOnce.Do(func() { close(l.ch) })
	return nil
}

// initStore 实现 Initialize(Options) 接口。
type initStore struct {
	*mockLocalStore
	initialized bool
}

func (s *initStore) Initialize(Options) { s.initialized = true }

func TestNewConfiguresMemStoreOptions(t *testing.T) {
	c := New(
		WithMemStore(),
		WithTTLJitter(5*time.Millisecond),
		WithSlidingWindow(10*time.Second),
	)
	assert.NotNil(t, c)
	c.Close()
}

func TestGetPropagatesStoreError(t *testing.T) {
	c := &cache{
		localStore: &failStore{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	var s string
	err := c.Get(context.Background(), "k", &s)
	assert.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError, "store error should propagate through Get")
}

func TestGetfnPropagatesStoreError(t *testing.T) {
	c := &cache{
		localStore: &failStore{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	var s string
	err := c.Getfn(context.Background(), "k", &s, func(ctx context.Context, key string, v any) (bool, error) {
		return false, nil
	}, 60)
	assert.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError)
}

func TestGetMultiStoreError(t *testing.T) {
	c := &cache{
		localStore: &failStore{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	_, err := c.GetMulti(context.Background(), "a", "b")
	assert.Error(t, err)
}

func TestGetMultiRawBytesFallback(t *testing.T) {
	local := newMemStore()
	_ = local.Put(context.Background(), "k", makeVersionedData(100, []byte("rawdata")), 60)
	c := &cache{
		localStore: local,
		serializer: jsonSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		ttl:        60,
		stopChan:   make(chan struct{}),
	}
	res, err := c.GetMulti(context.Background(), "k")
	assert.Nil(t, err)
	assert.Equal(t, "rawdata", res["k"], "non-JSON bytes should fall back to raw string")
}

func TestSetMultiBulkError(t *testing.T) {
	c := &cache{
		localStore: &bulkFailStore{mockLocalStore: newMockLocalStore()},
		serializer: jsonSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	err := c.SetMulti(context.Background(), map[string]any{"a": "1"}, 60)
	assert.Error(t, err)
}

func TestSetMultiBulkMarshalError(t *testing.T) {
	c := &cache{
		localStore: &bulkFailStore{mockLocalStore: newMockLocalStore()},
		serializer: failSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	err := c.SetMulti(context.Background(), map[string]any{"a": "1"}, 60)
	assert.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError)
}

func TestSetMultiFallbackPutError(t *testing.T) {
	c := &cache{
		localStore: &mockLocalPutFailStore{mockLocalStore: newMockLocalStore()},
		serializer: jsonSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	err := c.SetMulti(context.Background(), map[string]any{"a": "1"}, 60)
	assert.Error(t, err, "per-key fallback Put failure should propagate")
}

func TestSetMultiFallbackMarshalError(t *testing.T) {
	c := &cache{
		localStore: newMockLocalStore(),
		serializer: failSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	err := c.SetMulti(context.Background(), map[string]any{"a": "1"}, 60)
	assert.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError)
}

func TestPreLoadPutError(t *testing.T) {
	// PreLoad 批量写入：序列化失败 → 返回错误（首个失败即返回）
	c := &cache{
		localStore: newMemStore(), // 使 SetMulti 执行到 Marshal（failSerializer 报错）
		serializer: failSerializer{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	err := c.PreLoad(context.Background(), func(ctx context.Context) (map[string]any, error) {
		return map[string]any{"a": "1"}, nil
	}, 60)
	assert.Error(t, err)
}

func TestPutCacheLocalError(t *testing.T) {
	local := &mockLocalPutFailStore{mockLocalStore: newMockLocalStore()}
	c := &cache{
		localStore: local,
		serializer: jsonSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}
	err := c.Put(context.Background(), "k", "v", 60)
	assert.Error(t, err, "local store failure should propagate from Put")
}

// mockLocalPutFailStore 的 Put 恒失败（IsRemote=false）。
type mockLocalPutFailStore struct {
	*mockLocalStore
}

func (s *mockLocalPutFailStore) Put(_ context.Context, _ string, _ []byte, _ int) error {
	return assert.AnError
}

func TestSyncBatchRemoteGetError(t *testing.T) {
	local := newMemStore()
	_ = local.Put(context.Background(), "k", []byte("v"), 60)
	c := &cache{
		localStore:       local,
		remoteStore:      &failStore{},
		versionSyncBatch: defaultVersionSyncBatch,
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}
	// remote Get 出错 → continue，不 panic，本地数据保留
	c.syncBatch()
	_, exist, _ := local.Get(context.Background(), "k")
	assert.True(t, exist)
}

func TestRemoveFromStorageRemoteSuccess(t *testing.T) {
	remote := newTestRemoteStore()
	c := &cache{
		remoteStore: remote,
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	ctx := context.Background()
	_ = remote.Put(ctx, "k", []byte("v"), 60)

	c.degradeCount.Store(7)
	c.removeFromStorage(ctx, remote, "k")
	assert.Equal(t, int64(0), c.degradeCount.Load(), "successful remote delete should record success")
	_, exist, _ := remote.Get(ctx, "k")
	assert.False(t, exist)
}

func TestOptionsMethodWithStoreAndListener(t *testing.T) {
	var o Options
	o.WithStore(newMockLocalStore()) // IsRemote=false → local
	assert.NotNil(t, o.localStore)
	o.WithStore(newTestRemoteStore()) // IsRemote=true → remote
	assert.NotNil(t, o.remoteStore)
	o.WithListener(&failListener{})
	assert.NotNil(t, o.listener)
}

func TestOptionsInitCallsInitialize(t *testing.T) {
	s := &initStore{mockLocalStore: newMockLocalStore()}
	o := Options{localStore: s}
	o.init()
	assert.True(t, s.initialized, "Initialize should be invoked on stores implementing it")
}

func TestCloseClosesRemoteStore(t *testing.T) {
	remote := newMemStore() // 实现 Close()
	c := &cache{
		localStore:  newMemStore(),
		remoteStore: remote,
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	assert.NotPanics(t, func() { c.Close() })
}

func TestWatcherExitsOnChannelClose(t *testing.T) {
	lis := &closableListener{ch: make(chan string, 4)}
	ml := &mockLogger{}
	c := &cache{
		listener:   lis,
		localStore: newMemStore(),
		logger:     slog.New(ml),
		stopChan:   make(chan struct{}),
	}
	done := make(chan struct{})
	c.watcherWG.Add(1)
	go func() {
		c.startWatcher()
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)   // 等待 watcher 订阅
	_ = lis.Close(context.Background()) // 关闭 channel → watcher 退出

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not exit after channel close")
	}
}

// --- mem_store 分支补全 ---

func TestMemStoreSlidingWindow(t *testing.T) {
	s := newMemStore()
	s.slidingWindow = 5 * time.Second
	ctx := context.Background()
	_ = s.Put(ctx, "k", []byte("v"), 1) // 1s TTL

	time.Sleep(600 * time.Millisecond) // remaining ≈ 0.4s < 5s → 延长 TTL
	_, exist, _ := s.Get(ctx, "k")
	assert.True(t, exist)

	s.RLock()
	item, _ := s.items["k"]
	s.RUnlock()
	assert.Greater(t, item.Expiration-time.Now().UnixNano(), int64(700*time.Millisecond),
		"sliding window should extend the TTL")
}

func TestMemStorePutTTLJitter(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 100 * time.Millisecond
	_ = s.Put(context.Background(), "k", []byte("v"), 60)

	s.RLock()
	item, _ := s.items["k"]
	s.RUnlock()
	remaining := item.Expiration - time.Now().UnixNano()
	// jitter 加在 TTL 之上：60s ≤ remaining < 60s+100ms
	assert.Greater(t, remaining, int64(59*time.Second))
	assert.Less(t, remaining, int64(60*time.Second)+int64(200*time.Millisecond))
}

func TestEvictIfNeededRemovesExpired(t *testing.T) {
	s := newMemStore()
	s.maxItems = 2
	ctx := context.Background()
	_ = s.Put(ctx, "a", []byte("1"), 1)  // 1s TTL
	time.Sleep(1100 * time.Millisecond)  // 过期
	_ = s.Put(ctx, "b", []byte("2"), 60) // 触发 evictIfNeeded → 清理过期的 a
	assert.Equal(t, 1, s.Len())
}

func TestEvictIfNeededMaxBytes(t *testing.T) {
	s := newMemStore()
	s.maxBytes = 20
	ctx := context.Background()
	_ = s.Put(ctx, "a", []byte("12345678901234567890"), 60) // 20 bytes
	_ = s.Put(ctx, "b", []byte("12345678901234567890"), 60) // 40 > 20 → 淘汰 a
	assert.Equal(t, 1, s.Len())
	_, exist, _ := s.Get(ctx, "b")
	assert.True(t, exist)
}

func TestMemStoreSetMultiOverwrite(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()
	_ = s.Put(ctx, "a", []byte("old"), 60)
	_ = s.SetMulti(ctx, map[string][]byte{"a": []byte("new")}, 60)

	s.RLock()
	item, _ := s.items["a"]
	s.RUnlock()
	assert.Equal(t, []byte("new"), item.Value)
}

func TestMemStoreGetExpiredRemoves(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()
	_ = s.Put(ctx, "k", []byte("v"), 1)
	time.Sleep(1100 * time.Millisecond)

	_, exist, _ := s.Get(ctx, "k")
	assert.False(t, exist)

	s.RLock()
	_, ok := s.items["k"]
	s.RUnlock()
	assert.False(t, ok, "expired entry should be removed on access")
}

// --- P0-1: WithLogger 注入生效 ---

func TestWithLoggerInjection(t *testing.T) {
	ml := &mockLogger{}
	remote := &deleteFailStore{testRemoteStore: newTestRemoteStore()}
	c := New(
		WithLogger(slog.New(ml)),
		func(o *Options) { o.WithStore(remote) },
	)

	// 触发 Warn：remote Delete 失败
	c.Delete(context.Background(), "k")
	assert.Len(t, ml.warns, 1, "injected logger must receive the Warn call")
	assert.Contains(t, ml.warns[0], "delete from store")

	c.Close()
}

// --- P0-3: 裸字节 []byte 往返 ---

func TestPutBytesGetStringRoundTrip(t *testing.T) {
	c := New(WithMemStore())
	ctx := context.Background()

	// Marshal 对 []byte 裸存 → Get(*string) 应回退原始字节串
	_ = c.Put(ctx, "k", []byte("abc"), 60)
	var s string
	assert.Nil(t, c.Get(ctx, "k", &s))
	assert.Equal(t, "abc", s)

	// GetMulti 路径一致
	res, err := c.GetMulti(ctx, "k")
	assert.Nil(t, err)
	assert.Equal(t, "abc", res["k"])

	c.Close()
}

// --- P1-1: GetMulti/SetMulti BulkStore 闭环 ---

// bulkSpyStore 实现 BulkStore 并记录分派调用。
type bulkSpyStore struct {
	*mockLocalStore
	getMultiCalled bool
	setMultiCalled bool
	raw            map[string][]byte
}

func (s *bulkSpyStore) GetMulti(_ context.Context, keys ...string) (map[string][]byte, error) {
	s.getMultiCalled = true
	res := make(map[string][]byte, len(keys))
	for _, k := range keys {
		if v, ok := s.data[k]; ok {
			res[k] = v
		}
	}
	return res, nil
}

func (s *bulkSpyStore) SetMulti(_ context.Context, items map[string][]byte, _ int) error {
	s.setMultiCalled = true
	s.raw = items
	for k, v := range items {
		s.data[k] = v
	}
	return nil
}

func TestGetMultiDispatchesToBulkStore(t *testing.T) {
	local := &bulkSpyStore{mockLocalStore: newMockLocalStore()}
	local.data["a"] = wrapVersion([]byte(`"va"`))
	c := &cache{
		localStore: local,
		serializer: jsonSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		ttl:        60,
		stopChan:   make(chan struct{}),
	}

	res, err := c.GetMulti(context.Background(), "a", "miss")
	assert.Nil(t, err)
	assert.True(t, local.getMultiCalled, "GetMulti should dispatch to BulkStore")
	assert.Equal(t, "va", res["a"], "version prefix should be stripped")
	_, ok := res["miss"]
	assert.False(t, ok, "missed keys fall back to single-key path and stay absent")
}

func TestSetMultiBulkWrapsVersion(t *testing.T) {
	local := &bulkSpyStore{mockLocalStore: newMockLocalStore()}
	c := &cache{
		localStore: local,
		serializer: jsonSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		stopChan:   make(chan struct{}),
	}

	err := c.SetMulti(context.Background(), map[string]any{"a": "va"}, 60)
	assert.Nil(t, err)
	assert.True(t, local.setMultiCalled)

	raw, ok := local.raw["a"]
	assert.True(t, ok)
	assert.True(t, versionOf(raw) > 0, "bulk written data should carry version prefix")
	assert.Equal(t, []byte(`"va"`), payloadOf(raw))
}

// --- P0-2: verify 路径跳过 pending key 驱逐 ---

func TestVerifySkipsEvictionForPendingKey(t *testing.T) {
	local := newMemStore()
	// putFailStore：flush 失败 → pending 保留，模拟"恢复后补偿写仍失败"的场景
	remote := &putFailStore{testRemoteStore: newTestRemoteStore()}
	c := &cache{
		localStore:  local,
		remoteStore: remote,
		verifyEvery: 1,
		stats:       newStats(),
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		ttl:         60,
		stopChan:    make(chan struct{}),
	}
	ctx := context.Background()
	_ = local.Put(ctx, "k", makeVersionedData(100, []byte(`"v"`)), 60)

	// 模拟降级期间的 pending 写入
	c.pendingMu.Lock()
	if c.pendingWrites == nil {
		c.pendingWrites = make(map[string]pendingWrite)
	}
	c.pendingWrites["k"] = pendingWrite{data: []byte(`"v"`), expireSeconds: 60}
	c.pendingMu.Unlock()

	data, exist, err := c.getFromCache(ctx, "k", 0)
	assert.Nil(t, err)
	assert.True(t, exist, "pending key must not be evicted by verify")
	assert.Equal(t, []byte(`"v"`), data)

	// flush 失败 → pending 仍保留
	assert.Len(t, c.pendingWrites, 1, "failed flush should keep the pending write")

	_, localExist, _ := local.Get(ctx, "k")
	assert.True(t, localExist)
}

// --- P1-3: Update 与 Getfn 的 singleflight 互斥 ---

func TestUpdateConcurrentGetfnSharesSingleflight(t *testing.T) {
	c := &cache{
		localStore: newMemStore(),
		serializer: jsonSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		ttl:        60,
		stopChan:   make(chan struct{}),
	}
	ctx := context.Background()

	updateStarted := make(chan struct{})
	releaseUpdate := make(chan struct{})
	updateErr := make(chan error, 1)

	go func() {
		close(updateStarted)
		updateErr <- c.Update(ctx, "k", func(ctx context.Context, key string) error {
			<-releaseUpdate // 阻塞，保证 Update 持有 singleflight
			return nil
		})
	}()
	<-updateStarted
	time.Sleep(20 * time.Millisecond)

	// 并发 Getfn：应共享 Update 的 singleflight（key:%s 一致），不执行 loadFn
	loadCount := 0
	getfnErr := make(chan error, 1)
	go func() {
		var s string
		getfnErr <- c.Getfn(ctx, "k", &s, func(ctx context.Context, key string, v any) (bool, error) {
			loadCount++
			return true, nil
		}, 60)
	}()
	// 等待 Getfn 完成 getFromCache（本地 miss，微秒级）并进入 getFromSource 的
	// singleflight 阻塞；Update 仍在持锁，Getfn 只能共享其结果。
	time.Sleep(100 * time.Millisecond)

	close(releaseUpdate) // 放行 Update
	assert.Nil(t, <-updateErr)
	assert.ErrorIs(t, <-getfnErr, ErrEntityNotExist, "Getfn should share Update result (deleted) instead of loading")
	assert.Equal(t, 0, loadCount, "loadFn must not run while Update holds the singleflight")

	// Update 完成后 Getfn 正常回源
	var s2 string
	assert.Nil(t, c.Getfn(ctx, "k", &s2, func(ctx context.Context, key string, v any) (bool, error) {
		if sv, ok := v.(*string); ok {
			*sv = "fresh"
		}
		return true, nil
	}, 60))
	assert.Equal(t, "fresh", s2)
}

// --- P2: mem_store.Close 幂等 ---

func TestMemStoreCloseIdempotent(t *testing.T) {
	s := newMemStore()
	s.Close()
	assert.NotPanics(t, func() { s.Close() }, "second Close must not panic")
}

// --- 覆盖率补全：GetMulti 分派分支 / DeletePattern / pending 上限 ---

// bulkGetFailStore 的 GetMulti 恒失败。
type bulkGetFailStore struct {
	*mockLocalStore
}

func (s *bulkGetFailStore) GetMulti(_ context.Context, _ ...string) (map[string][]byte, error) {
	return nil, assert.AnError
}

func (s *bulkGetFailStore) SetMulti(_ context.Context, _ map[string][]byte, _ int) error {
	return nil
}

// patternFailStore 的 DeletePattern 恒失败。
type patternFailStore struct {
	*mockLocalStore
}

func (s *patternFailStore) DeletePattern(_ context.Context, _ string) error {
	return assert.AnError
}

func TestGetMultiBulkGetError(t *testing.T) {
	c := &cache{
		localStore: &bulkGetFailStore{mockLocalStore: newMockLocalStore()},
		serializer: jsonSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		ttl:        60,
		stopChan:   make(chan struct{}),
	}
	_, err := c.GetMulti(context.Background(), "a")
	assert.Error(t, err, "BulkStore GetMulti error should propagate")
}

func TestGetMultiBulkDispatchBranches(t *testing.T) {
	// 分派路径：a 正常命中、p 为 placeholder（跳过）、miss 回退单值路径（remote 出错）
	local := &bulkSpyStore{mockLocalStore: newMockLocalStore()}
	local.data["a"] = wrapVersion([]byte(`"va"`))
	local.data["p"] = wrapVersion([]byte("*"))
	c := &cache{
		localStore:          local,
		remoteStore:         &failStore{},
		serializer:          jsonSerializer{},
		notExistPlaceholder: []byte("*"),
		stats:               newStats(),
		metrics:             noopMetrics{},
		logger:              slog.Default(),
		ttl:                 60,
		stopChan:            make(chan struct{}),
	}
	_, err := c.GetMulti(context.Background(), "a", "p", "miss")
	assert.Error(t, err, "missed key fallback should propagate single-path store error")
}

func TestGetMultiBulkDispatchFallbackHit(t *testing.T) {
	// 分派未命中 key 从 remote 补回
	local := &bulkSpyStore{mockLocalStore: newMockLocalStore()}
	local.data["a"] = wrapVersion([]byte(`"va"`))
	remote := newTestRemoteStore()
	remote.data["only-remote"] = wrapVersion([]byte(`"rv"`))
	c := &cache{
		localStore:  local,
		remoteStore: remote,
		serializer:  jsonSerializer{},
		stats:       newStats(),
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		ttl:         60,
		stopChan:    make(chan struct{}),
	}
	res, err := c.GetMulti(context.Background(), "a", "only-remote")
	assert.Nil(t, err)
	assert.Equal(t, "va", res["a"])
	assert.Equal(t, "rv", res["only-remote"], "missed key should be filled via single-path fallback")
}

func TestGetMultiBulkDispatchFallbackRawBytes(t *testing.T) {
	// 分派未命中 key 从 remote 补回裸字节（回退循环 unmarshal fallback）
	local := &bulkSpyStore{mockLocalStore: newMockLocalStore()}
	remote := newTestRemoteStore()
	remote.data["raw"] = []byte("rawdata") // 裸字节，无版本前缀
	c := &cache{
		localStore:  local,
		remoteStore: remote,
		serializer:  jsonSerializer{},
		stats:       newStats(),
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		ttl:         60,
		stopChan:    make(chan struct{}),
	}
	res, err := c.GetMulti(context.Background(), "raw")
	assert.Nil(t, err)
	assert.Equal(t, "rawdata", res["raw"], "bare bytes should fall back to raw string in fallback loop")
}

func TestGetMultiFallbackLoop(t *testing.T) {
	// 非 BulkStore 的 local → 逐 key 循环（含裸字节 fallback）
	local := newMockLocalStore()
	local.data["a"] = wrapVersion([]byte(`"va"`))
	local.data["b"] = []byte("rawdata")
	c := &cache{
		localStore: local,
		serializer: jsonSerializer{},
		stats:      newStats(),
		metrics:    noopMetrics{},
		logger:     slog.Default(),
		ttl:        60,
		stopChan:   make(chan struct{}),
	}
	res, err := c.GetMulti(context.Background(), "a", "b", "miss")
	assert.Nil(t, err)
	assert.Equal(t, "va", res["a"])
	assert.Equal(t, "rawdata", res["b"], "bare bytes should fall back to raw string")
	_, ok := res["miss"]
	assert.False(t, ok)
}

func TestDeletePatternErrorWarns(t *testing.T) {
	ml := &mockLogger{}
	c := &cache{
		localStore: &patternFailStore{mockLocalStore: newMockLocalStore()},
		logger:     slog.New(ml),
		stopChan:   make(chan struct{}),
	}
	c.DeletePattern(context.Background(), "x:*")
	assert.Len(t, ml.warns, 1, "pattern delete failure should be logged via Warn")
	assert.Contains(t, ml.warns[0], "delete pattern")
}

func TestPendingWritesLimit(t *testing.T) {
	remote := newTestRemoteStore()
	c := &cache{
		localStore:  newMemStore(),
		remoteStore: remote,
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)
	ctx := context.Background()

	// 填满 pending 上限
	c.pendingMu.Lock()
	if c.pendingWrites == nil {
		c.pendingWrites = make(map[string]pendingWrite)
	}
	for i := 0; i < maxPendingWrites; i++ {
		c.pendingWrites[fmt.Sprintf("k%d", i)] = pendingWrite{data: []byte("v"), expireSeconds: 60}
	}
	c.pendingMu.Unlock()

	// 超限写入被拒绝并返回明确错误
	err := c.putCache(ctx, "overflow", []byte("v"), 60)
	assert.ErrorIs(t, err, ErrPendingWritesFull, "write beyond the limit should return ErrPendingWritesFull")
	c.pendingMu.Lock()
	assert.Len(t, c.pendingWrites, maxPendingWrites)
	_, ok := c.pendingWrites["overflow"]
	c.pendingMu.Unlock()
	assert.False(t, ok, "write beyond the limit should be rejected")
}

// --- P0-1: 降级期间 Delete 缓冲（pendingDeletes）---

func TestDegradedDeleteFlushedAfterRecovery(t *testing.T) {
	remote := newTestRemoteStore()
	local := newMemStore()
	c := &cache{
		localStore:  local,
		remoteStore: remote,
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	ctx := context.Background()

	// remote 已有数据（模拟其它实例写入）
	_ = remote.Put(ctx, "k", makeVersionedData(100, []byte("v")), 60)

	c.degraded.Store(true)
	// 降级期间 Delete：remote 删除进入 pendingDeletes
	c.Delete(ctx, "k")
	c.pendingMu.Lock()
	_, inDeletes := c.pendingDeletes["k"]
	c.pendingMu.Unlock()
	assert.True(t, inDeletes, "delete should be buffered while degraded")

	// 恢复：flush 删除 → remote 数据不存在（不复活）
	c.recordRemoteSuccess()
	_, remoteExist, _ := remote.Get(ctx, "k")
	assert.False(t, remoteExist, "deleted key must not resurrect after recovery")
}

func TestDegradedPutThenDeleteFlushesDelete(t *testing.T) {
	remote := newTestRemoteStore()
	local := newMemStore()
	c := &cache{
		localStore:  local,
		remoteStore: remote,
		serializer:  jsonSerializer{},
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)
	ctx := context.Background()

	// 降级期 Put → pendingWrites["k"]
	assert.Nil(t, c.Put(ctx, "k", "v", 60))
	// 降级期 Delete → 从 pendingWrites 移除，改记 pendingDeletes（删除优先）
	c.Delete(ctx, "k")

	c.pendingMu.Lock()
	_, inWrites := c.pendingWrites["k"]
	_, inDeletes := c.pendingDeletes["k"]
	c.pendingMu.Unlock()
	assert.False(t, inWrites, "delete should supersede pending write")
	assert.True(t, inDeletes)

	// 恢复：flush → remote 无数据（删除优先于写）
	c.recordRemoteSuccess()
	_, remoteExist, _ := remote.Get(ctx, "k")
	assert.False(t, remoteExist, "delete must win over write after recovery")
}

// --- P0-2: flush 退避 ---

func TestFlushBackoffSkipsFrequentRetries(t *testing.T) {
	remote := &putFailStore{testRemoteStore: newTestRemoteStore()}
	c := &cache{
		remoteStore: remote,
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)
	ctx := context.Background()

	_ = c.putCache(ctx, "k", []byte("v"), 60)
	c.recordRemoteSuccess() // 转换点强制 flush → 失败 → pending 保留 + lastFlush 更新

	// 非转换点立即再次触发：退避期内不执行 flush
	c.pendingMu.Lock()
	c.lastFlush = time.Now()
	c.pendingMu.Unlock()
	c.recordRemoteSuccess() // wasDegraded=false → 退避检查
	c.pendingMu.Lock()
	kept := len(c.pendingWrites)
	c.pendingMu.Unlock()
	assert.Equal(t, 1, kept, "flush should be throttled by backoff interval")
}

// --- P0-3: context.Canceled 不计入降级 ---

func TestRecordRemoteErrorIgnoresCanceled(t *testing.T) {
	c := &cache{
		degradeThreshold: 2,
		metrics:          noopMetrics{},
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}
	// 用户主动取消连续多次 → 不进入降级
	for i := 0; i < 5; i++ {
		c.recordRemoteError(context.Canceled)
	}
	assert.False(t, c.isDegraded())
	assert.Equal(t, int64(0), c.degradeCount.Load(), "Canceled must not count toward degrade")
}

func TestRecordRemoteErrorCountsDeadlineExceeded(t *testing.T) {
	c := &cache{
		degradeThreshold: 2,
		metrics:          noopMetrics{},
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}
	c.recordRemoteError(context.DeadlineExceeded)
	assert.False(t, c.isDegraded())
	c.recordRemoteError(context.DeadlineExceeded)
	assert.True(t, c.isDegraded(), "DeadlineExceeded should count toward degraded mode")
}

// --- P1-1: SetMulti TTL jitter ---

func TestMemStoreSetMultiTTLJitter(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 100 * time.Millisecond
	ctx := context.Background()
	_ = s.SetMulti(ctx, map[string][]byte{"a": []byte("1"), "b": []byte("2")}, 60)

	s.RLock()
	ia := s.items["a"]
	ib := s.items["b"]
	s.RUnlock()

	// jitter 生效：每个 key 独立 jitter，TTL 位于 [60s, 60s+100ms) 有效域
	for _, exp := range []int64{ia.Expiration, ib.Expiration} {
		remaining := exp - time.Now().UnixNano()
		assert.Greater(t, remaining, int64(59*time.Second))
		assert.Less(t, remaining, int64(60*time.Second)+int64(200*time.Millisecond))
	}
}

// --- P1-2: Stats.Clear 清 Shared ---

func TestStatsClearResetsShared(t *testing.T) {
	s := newStats()
	s.IncrShared()
	s.IncrShared()
	s.Clear()
	assert.Equal(t, uint64(0), s.Shared, "Clear should reset Shared counter")
}

// --- P1-3: Delete 空参数 ---

type countingDeleteStore struct {
	*testRemoteStore
	deletes int
}

func (s *countingDeleteStore) Delete(_ context.Context, _ ...string) error {
	s.deletes++
	return nil
}

func TestDeleteEmptyKeysNoOp(t *testing.T) {
	remote := &countingDeleteStore{testRemoteStore: newTestRemoteStore()}
	ml := &mockLogger{}
	c := &cache{
		localStore:  newMemStore(),
		remoteStore: remote,
		logger:      slog.New(ml),
		stopChan:    make(chan struct{}),
	}
	c.Delete(context.Background()) // 空参数
	assert.Equal(t, 0, remote.deletes, "no store delete should be issued for empty keys")
	assert.Empty(t, ml.warns, "no warn noise for empty delete")
}

// --- P2: pending 满返回错误 / 未定义行为 ---

// TestPendingWritesLimit 断言超限时返回 ErrPendingWritesFull（见下，更新版在文件后部）

func TestPutNilAndEmptyBytes(t *testing.T) {
	c := New(WithMemStore())
	ctx := context.Background()

	// Put nil → 存空数据；Get 成功返回（v 保持零值）
	assert.Nil(t, c.Put(ctx, "nil-key", nil, 60))
	var s1 string
	assert.Nil(t, c.Get(ctx, "nil-key", &s1), "nil value should be readable without error")
	assert.Equal(t, "", s1)

	// Put []byte{} → 同样语义
	assert.Nil(t, c.Put(ctx, "empty-key", []byte{}, 60))
	var s2 string
	assert.Nil(t, c.Get(ctx, "empty-key", &s2))
	assert.Equal(t, "", s2)

	c.Close()
}

func TestGetMultiFallbackFiltersPlaceholder(t *testing.T) {
	local := newMockLocalStore()
	local.data["p"] = []byte("*") // 无版本前缀的裸 placeholder
	c := &cache{
		localStore:          local,
		serializer:          jsonSerializer{},
		notExistPlaceholder: []byte("*"),
		stats:               newStats(),
		metrics:             noopMetrics{},
		logger:              slog.Default(),
		ttl:                 60,
		stopChan:            make(chan struct{}),
	}
	res, err := c.GetMulti(context.Background(), "p")
	assert.Nil(t, err)
	_, ok := res["p"]
	assert.False(t, ok, "placeholder should be filtered from GetMulti results")
}

// --- P0-1/P0-2: flush 删除失败保留重试 ---

func TestFlushPendingDeleteFailureKeepsForRetry(t *testing.T) {
	remote := &deleteFailStore{testRemoteStore: newTestRemoteStore()}
	c := &cache{
		remoteStore: remote,
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)
	ctx := context.Background()

	c.Delete(ctx, "k") // 降级 → pendingDeletes["k"]

	// 转换点 flush：Delete 失败 → 保留待重试，并推进降级计数
	c.recordRemoteSuccess()
	c.pendingMu.Lock()
	_, kept := c.pendingDeletes["k"]
	c.pendingMu.Unlock()
	assert.True(t, kept, "failed delete flush should keep the pending delete")
	assert.GreaterOrEqual(t, c.degradeCount.Load(), int64(1), "flush failure should advance degrade counter")

	// remote 恢复后重试成功 → pending 清空
	c.remoteStore = newTestRemoteStore()
	c.pendingMu.Lock()
	c.lastFlush = time.Time{}
	c.pendingMu.Unlock()
	c.recordRemoteSuccess()
	c.pendingMu.Lock()
	assert.Len(t, c.pendingDeletes, 0, "retry should clear pending deletes")
	c.pendingMu.Unlock()
}

// --- L1 回写失败可观测化（Warn 日志，控制流不变）---

func TestWriteBackFailureWarns(t *testing.T) {
	// local 写失败（容量满/配置错误模拟）+ remote 命中：
	// 回写失败仅 Warn，Get 仍成功返回数据（控制流不变）。
	local := &mockLocalPutFailStore{mockLocalStore: newMockLocalStore()}
	remote := newTestRemoteStore()
	ml := &mockLogger{}
	c := &cache{
		localStore:          local,
		remoteStore:         remote,
		serializer:          jsonSerializer{},
		notExistPlaceholder: []byte("*"),
		stats:               newStats(),
		metrics:             noopMetrics{},
		logger:              slog.New(ml),
		ttl:                 60,
		stopChan:            make(chan struct{}),
	}
	ctx := context.Background()
	_ = remote.Put(ctx, "k", makeVersionedData(200, []byte(`"v"`)), 60)

	var s string
	err := c.Get(ctx, "k", &s)
	assert.Nil(t, err, "control flow unchanged: Get must still succeed")
	assert.Equal(t, "v", s)
	assert.Len(t, ml.warns, 1, "write-back failure should be logged")
	assert.Contains(t, ml.warns[0], "write back to local store")
}

func TestGetfnFillFailureWarns(t *testing.T) {
	// local 写失败：Getfn 回填失败仅 Warn，仍返回 fn 结果（控制流不变）。
	local := &mockLocalPutFailStore{mockLocalStore: newMockLocalStore()}
	ml := &mockLogger{}
	c := &cache{
		localStore:          local,
		serializer:          jsonSerializer{},
		notExistPlaceholder: []byte("*"),
		stats:               newStats(),
		metrics:             noopMetrics{},
		logger:              slog.New(ml),
		ttl:                 60,
		stopChan:            make(chan struct{}),
	}
	ctx := context.Background()

	var s string
	err := c.Getfn(ctx, "k", &s, func(ctx context.Context, key string, v any) (bool, error) {
		if sv, ok := v.(*string); ok {
			*sv = "loaded"
		}
		return true, nil
	}, 60)
	assert.Nil(t, err, "fill failure must not block the fn result")
	assert.Equal(t, "loaded", s)
	assert.Len(t, ml.warns, 1, "fill failure should be logged")
	assert.Contains(t, ml.warns[0], "fill cache")

	// 占位符回填路径同样 Warn（exist=false 走同一 putCache）
	var s2 string
	err = c.Getfn(ctx, "missing", &s2, func(ctx context.Context, key string, v any) (bool, error) {
		return false, nil
	}, 60)
	assert.ErrorIs(t, err, ErrEntityNotExist)
	assert.Len(t, ml.warns, 2, "placeholder fill failure should also be logged")
}

// --- TTL jitter 默认开启（可显式关闭）---

func TestTTLJitterDefaultEnabled(t *testing.T) {
	// 零配置（不调 WithTTLJitter）：默认抖动生效（0~defaultTTLJitter）
	c := New(WithMemStore())
	ms, ok := c.localStore.(*mem_store)
	assert.True(t, ok)
	assert.Equal(t, defaultTTLJitter, ms.ttlJitter, "anti-avalanche jitter should be on by default")

	// 行为验证：Put 后条目过期时间落在 [TTL, TTL+defaultTTLJitter] 区间
	ctx := context.Background()
	_ = c.Put(ctx, "k", "v", 60)
	ms.RLock()
	item, ok := ms.items["k"]
	ms.RUnlock()
	assert.True(t, ok)
	remaining := item.Expiration - time.Now().UnixNano()
	assert.GreaterOrEqual(t, remaining, int64(59*time.Second))
	assert.LessOrEqual(t, remaining, int64(60*time.Second)+int64(defaultTTLJitter))

	c.Close()
}

func TestTTLJitterExplicitZeroDisables(t *testing.T) {
	// 显式 WithTTLJitter(0)：关闭抖动（默认值不得覆盖）
	c := New(WithMemStore(), WithTTLJitter(0))
	ms, ok := c.localStore.(*mem_store)
	assert.True(t, ok)
	assert.Equal(t, time.Duration(0), ms.ttlJitter, "explicit WithTTLJitter(0) must disable jitter")

	// 行为验证：过期时间为精确 TTL（无叠加）
	ctx := context.Background()
	_ = c.Put(ctx, "k", "v", 60)
	ms.RLock()
	item, _ := ms.items["k"]
	ms.RUnlock()
	remaining := item.Expiration - time.Now().UnixNano()
	assert.GreaterOrEqual(t, remaining, int64(59*time.Second))
	assert.LessOrEqual(t, remaining, int64(60*time.Second)+int64(100*time.Millisecond))

	c.Close()
}

func TestTTLJitterCustomRange(t *testing.T) {
	// 自定义范围生效
	c := New(WithMemStore(), WithTTLJitter(5*time.Second))
	ms, _ := c.localStore.(*mem_store)
	assert.Equal(t, 5*time.Second, ms.ttlJitter)
	c.Close()
}

// --- P1-2: Getfn 免传 TTL（expireSeconds<=0 回落全局 WithTTL）---

func TestGetfnZeroExpireUsesGlobalTTL(t *testing.T) {
	// Getfn 传 0（或负值）→ 使用全局 WithTTL 值（与 Put 的 0=永不过期不同）
	c := New(WithMemStore(), WithTTL(120))
	ms, ok := c.localStore.(*mem_store)
	assert.True(t, ok)
	ctx := context.Background()

	var s string
	assert.Nil(t, c.Getfn(ctx, "k", &s, func(ctx context.Context, key string, v any) (bool, error) {
		if sv, ok := v.(*string); ok {
			*sv = "v"
		}
		return true, nil
	}, 0))
	assert.Equal(t, "v", s)

	// 回填 TTL 应为 120s（全局 WithTTL），而非默认 60s 或永不过期
	ms.RLock()
	item, ok := ms.items["k"]
	ms.RUnlock()
	assert.True(t, ok)
	remaining := item.Expiration - time.Now().UnixNano()
	assert.GreaterOrEqual(t, remaining, int64(119*time.Second), "fill TTL should use global TTL (120s)")
	assert.LessOrEqual(t, remaining, int64(120*time.Second)+int64(defaultTTLJitter)+int64(time.Second))

	// 负值同样回落全局 TTL
	var s2 string
	assert.Nil(t, c.Getfn(ctx, "k2", &s2, func(ctx context.Context, key string, v any) (bool, error) {
		if sv, ok := v.(*string); ok {
			*sv = "v2"
		}
		return true, nil
	}, -1))
	ms.RLock()
	item2, ok := ms.items["k2"]
	ms.RUnlock()
	assert.True(t, ok)
	remaining2 := item2.Expiration - time.Now().UnixNano()
	assert.GreaterOrEqual(t, remaining2, int64(119*time.Second))

	c.Close()
}

// --- SetMulti 降级 + remote bulk 缓冲（与单键 Put 语义一致）---

// countingBulkRemoteStore 是降级测试用 remote bulk store（记录 SetMulti 是否被调用）。
type countingBulkRemoteStore struct {
	*testRemoteStore
	setMultiCalled bool
}

func (s *countingBulkRemoteStore) GetMulti(_ context.Context, _ ...string) (map[string][]byte, error) {
	return nil, nil
}

func (s *countingBulkRemoteStore) SetMulti(_ context.Context, _ map[string][]byte, _ int) error {
	s.setMultiCalled = true
	return nil
}

func TestSetMultiDegradedRemoteBulkBuffers(t *testing.T) {
	// 降级 + remote bulk store：SetMulti 必须走 putInStore 的 pending 缓冲，
	// 不得绕过降级逻辑直接写远程（与单键 Put 语义一致）。
	remote := &countingBulkRemoteStore{testRemoteStore: newTestRemoteStore()}
	c := &cache{
		localStore:  newMemStore(),
		remoteStore: remote,
		serializer:  jsonSerializer{},
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)
	ctx := context.Background()

	err := c.SetMulti(ctx, map[string]any{"a": "1", "b": "2"}, 60)
	assert.Nil(t, err)
	assert.False(t, remote.setMultiCalled, "degraded remote bulk must not write directly")
	c.pendingMu.Lock()
	assert.Len(t, c.pendingWrites, 2, "bulk writes should be buffered while degraded")
	c.pendingMu.Unlock()
}

// --- Stats 只读快照（值拷贝）---

func TestStatsSnapshotIsolated(t *testing.T) {
	s := newStats()
	s.IncrHit("a")
	s.IncrHit("a")
	s.IncrMiss("b")
	s.IncrQuery()
	s.IncrShared()

	snap := s.Snapshot()
	assert.Equal(t, uint64(2), snap.TotalHits())
	assert.Equal(t, uint64(1), snap.TotalMiss())
	assert.Equal(t, uint64(1), snap.Query)
	assert.Equal(t, uint64(1), snap.Shared)

	// 修改/清空快照不影响内部计数
	snap.Query = 999
	snap.Shared = 999
	snap.Clear()

	assert.Equal(t, uint64(1), s.Query, "snapshot mutation must not affect internal counters")
	assert.Equal(t, uint64(1), s.Shared)
	assert.Equal(t, uint64(2), s.TotalHits(), "snapshot Clear must not clear internal counters")
	assert.Equal(t, uint64(1), s.TotalMiss())

	// 再次快照仍一致
	snap2 := s.Snapshot()
	assert.Equal(t, uint64(1), snap2.Query)
	assert.Equal(t, uint64(2), snap2.TotalHits())
}

// --- SetMulti bulk 路径 remote 降级计数（与单键 Put 对齐）---

// bulkRemoteFailStore 是 remote bulk store（IsRemote=true），SetMulti 恒失败。
type bulkRemoteFailStore struct {
	*testRemoteStore
}

func (s *bulkRemoteFailStore) GetMulti(_ context.Context, _ ...string) (map[string][]byte, error) {
	return nil, nil
}

func (s *bulkRemoteFailStore) SetMulti(_ context.Context, _ map[string][]byte, _ int) error {
	return assert.AnError
}

func TestSetMultiBulkRemoteErrorDrivesDegrade(t *testing.T) {
	remote := &bulkRemoteFailStore{testRemoteStore: newTestRemoteStore()}
	c := &cache{
		localStore:       newMemStore(),
		remoteStore:      remote,
		serializer:       jsonSerializer{},
		degradeThreshold: 2,
		metrics:          noopMetrics{},
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}
	ctx := context.Background()

	// 第 1 次：recordRemoteError(1) < 2，不降级
	err := c.SetMulti(ctx, map[string]any{"a": "1"}, 60)
	assert.Error(t, err)
	assert.False(t, c.isDegraded())

	// 第 2 次：recordRemoteError(2) >= 2 → 降级
	err = c.SetMulti(ctx, map[string]any{"b": "2"}, 60)
	assert.Error(t, err)
	assert.True(t, c.isDegraded(), "remote bulk failures should drive degraded mode")
}

func TestSetMultiBulkRemoteSuccessResetsDegrade(t *testing.T) {
	remote := &countingBulkRemoteStore{testRemoteStore: newTestRemoteStore()}
	c := &cache{
		localStore:       newMemStore(),
		remoteStore:      remote,
		serializer:       jsonSerializer{},
		degradeThreshold: 2,
		metrics:          noopMetrics{},
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}
	ctx := context.Background()

	// 计数推进但未降级（非降级状态才走 bulk 路径）
	c.degradeCount.Store(5)

	err := c.SetMulti(ctx, map[string]any{"a": "1"}, 60)
	assert.Nil(t, err)
	assert.Equal(t, int64(0), c.degradeCount.Load(), "bulk success should reset degrade counter")
	assert.False(t, c.isDegraded())
}

func TestSetMultiLocalBulkErrorDoesNotCount(t *testing.T) {
	// local bulk（IsRemote=false）失败与降级语义无关，不计数
	local := &bulkFailStore{mockLocalStore: newMockLocalStore()}
	c := &cache{
		localStore:       local,
		serializer:       jsonSerializer{},
		degradeThreshold: 2,
		metrics:          noopMetrics{},
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}
	ctx := context.Background()

	err := c.SetMulti(ctx, map[string]any{"a": "1"}, 60)
	assert.Error(t, err)
	assert.False(t, c.isDegraded(), "local bulk failure must not drive degraded mode")
	assert.Equal(t, int64(0), c.degradeCount.Load())
}

// --- P0-1: pendingDeletes 上限 ---

func TestPendingDeletesLimit(t *testing.T) {
	remote := newTestRemoteStore()
	ml := &mockLogger{}
	c := &cache{
		localStore:  newMemStore(),
		remoteStore: remote,
		metrics:     noopMetrics{},
		logger:      slog.New(ml),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)
	ctx := context.Background()

	// 填满 pendingDeletes 上限
	c.pendingMu.Lock()
	if c.pendingDeletes == nil {
		c.pendingDeletes = make(map[string]struct{})
	}
	for i := 0; i < maxPendingWrites; i++ {
		c.pendingDeletes[fmt.Sprintf("k%d", i)] = struct{}{}
	}
	c.pendingMu.Unlock()

	// 超限删除被拒 + Warn
	c.Delete(ctx, "overflow")
	c.pendingMu.Lock()
	assert.Len(t, c.pendingDeletes, maxPendingWrites)
	_, ok := c.pendingDeletes["overflow"]
	c.pendingMu.Unlock()
	assert.False(t, ok, "delete beyond the limit should be rejected")
	assert.Len(t, ml.warns, 1, "rejected delete should be logged")
	assert.Contains(t, ml.warns[0], "pending deletes full")
}

// --- P0-2: flush 摘取窗口保持 hasPending 保护 ---

// slowFlushRemoteStore 的 Put 慢（扩大 flush 锁外 IO 窗口），并通知测试已进入 IO。
type slowFlushRemoteStore struct {
	*testRemoteStore
	started chan struct{}
}

func (s *slowFlushRemoteStore) Put(ctx context.Context, key string, v []byte, expireSecond int) error {
	select {
	case <-s.started:
	default:
		close(s.started)
	}
	time.Sleep(200 * time.Millisecond)
	return s.testRemoteStore.Put(ctx, key, v, expireSecond)
}

func TestFlushWindowKeepsPendingProtection(t *testing.T) {
	remote := &slowFlushRemoteStore{testRemoteStore: newTestRemoteStore(), started: make(chan struct{})}
	local := newMemStore()
	c := &cache{
		localStore:  local,
		remoteStore: remote,
		metrics:     noopMetrics{},
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
	c.degraded.Store(true)
	ctx := context.Background()

	// 降级期写入 pending
	_ = c.putCache(ctx, "k", []byte("v"), 60)
	assert.Len(t, c.pendingWrites, 1)

	// 触发 flush（转换点 force）→ 锁外 IO 开始（Put 慢）
	done := make(chan struct{})
	go func() {
		c.recordRemoteSuccess() // wasDegraded=true → flushPending(true)
		close(done)
	}()

	<-remote.started // flush 已进入锁外 IO（pending 已摘取到 flushing）
	// flush 进行中：hasPending("k") 仍为 true（不驱逐）
	assert.True(t, c.hasPending("k"), "flush window must keep pending protection")

	// syncBatch 不驱逐该 key
	c.syncBatch()
	_, localExist, _ := local.Get(ctx, "k")
	assert.True(t, localExist, "local must not be evicted during flush window")

	<-done
	// flush 完成后：pending 清空、flushing 清空
	c.pendingMu.Lock()
	assert.Len(t, c.pendingWrites, 0)
	assert.Len(t, c.flushing, 0, "flushing placeholders must be cleaned after flush")
	c.pendingMu.Unlock()
}

// --- 部署模式：只远程 / 默认注入 / 显式本地 ---

func TestRemoteOnlyMode(t *testing.T) {
	// 显式只配远程（WithStore(remoteStore)）→ 不注入本地缓存（localStore 为 nil）
	remote := newTestRemoteStore()
	c := New(func(o *Options) { o.WithStore(remote) })
	assert.Nil(t, c.localStore, "remote-only mode must not inject local store")
	assert.NotNil(t, c.remoteStore)

	ctx := context.Background()
	// Put/Get 走 remote
	assert.Nil(t, c.Put(ctx, "k", "v", 60))
	var s string
	assert.Nil(t, c.Get(ctx, "k", &s))
	assert.Equal(t, "v", s)

	// Getfn 回源正常
	var s2 string
	assert.Nil(t, c.Getfn(ctx, "k2", &s2, func(ctx context.Context, key string, v any) (bool, error) {
		if sv, ok := v.(*string); ok {
			*sv = "loaded"
		}
		return true, nil
	}, 60))
	assert.Equal(t, "loaded", s2)

	// Delete 走 remote
	c.Delete(ctx, "k")
	_, exist, _ := remote.Get(ctx, "k")
	assert.False(t, exist, "delete should reach remote in remote-only mode")
	c.Close()
}

func TestDefaultMemStoreInjection(t *testing.T) {
	// 零参 New → 默认注入 mem_store（不回归）
	c := New()
	_, ok := c.localStore.(*mem_store)
	assert.True(t, ok, "zero-arg New should inject mem_store")
	c.Close()
}

func TestExplicitLocalStoreNoInjection(t *testing.T) {
	// 显式 WithStore(localStore) → 使用传入 store，不注入默认
	local := newMockLocalStore()
	c := New(func(o *Options) { o.WithStore(local) })
	assert.Equal(t, local, c.localStore, "explicit local store must be used, not injected")
	c.Close()
}
