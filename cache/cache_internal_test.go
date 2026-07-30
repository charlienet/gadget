package cache

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/charlienet/gadget/logger"
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

func (s *testRemoteStore) Name() string  { return "test-remote" }
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
	local.Put(ctx, "key", makeVersionedData(100, []byte("old")), 60)
	remote.Put(ctx, "key", makeVersionedData(200, []byte("new")), 60)

	c := &cache{
		localStore:       local,
		remoteStore:      remote,
		ttl:              60,
		versionSyncBatch: defaultVersionSyncBatch,
		logger:           logger.DefaultLogger,
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
	local.Put(ctx, "gone", []byte("stale"), 60)
	// remote does NOT have "gone"

	c := &cache{
		localStore:       local,
		remoteStore:      remote,
		versionSyncBatch: defaultVersionSyncBatch,
		logger:           logger.DefaultLogger,
		stopChan:         make(chan struct{}),
	}

	c.syncBatch()

	_, exist, _ := local.Get(ctx, "gone")
	assert.False(t, exist)
}

func TestSyncBatchSkipsWhenDegraded(t *testing.T) {
	local := newMemStore()
	remote := newTestRemoteStore()

	local.Put(context.Background(), "k", makeVersionedData(100, []byte("v")), 60)
	remote.Put(context.Background(), "k", makeVersionedData(200, []byte("changed")), 60)

	c := &cache{
		localStore:  local,
		remoteStore: remote,
		logger:      logger.DefaultLogger,
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
		logger:      logger.DefaultLogger,
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
		logger:           logger.DefaultLogger,
		stopChan:         make(chan struct{}),
	}

	// Empty store → cursor reset to 0, no error
	c.syncBatch()
	assert.Equal(t, 0, c.versionCursor)

	// Put some items, sync once
	local.Put(context.Background(), "a", []byte("1"), 60)
	local.Put(context.Background(), "b", []byte("2"), 60)
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
	local.Put(ctx, "key", makeVersionedData(100, []byte("old")), 60)
	remote.Put(ctx, "key", makeVersionedData(200, []byte("new")), 60)

	c := &cache{
		localStore:          local,
		remoteStore:         remote,
		ttl:                 60,
		logger:              logger.DefaultLogger,
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

	s.Put(ctx, "a", []byte("1"), 60)
	s.Put(ctx, "b", []byte("2"), 60)
	s.Put(ctx, "c", []byte("3"), 60)

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

	s.Put(ctx, "user:1", []byte("a"), 60)
	s.Put(ctx, "user:2", []byte("b"), 60)
	s.Put(ctx, "admin:1", []byte("c"), 60)

	s.DeletePattern(ctx, "user:*")

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

	s.Put(ctx, "a", []byte("1"), 60)
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
		logger:     logger.DefaultLogger,
		stopChan:   make(chan struct{}),
	}
	ctx := context.Background()

	local.Put(ctx, "x:1", []byte("v1"), 60)
	local.Put(ctx, "x:2", []byte("v2"), 60)
	local.Put(ctx, "y:1", []byte("v3"), 60)

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
	c.localStore.Put(ctx, "k", []byte("v"), 60)
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
		logger:           logger.DefaultLogger,
		stopChan:         make(chan struct{}),
	}

	assert.False(t, c.isDegraded())

	c.recordRemoteError()
	assert.False(t, c.isDegraded())

	c.recordRemoteError()
	assert.False(t, c.isDegraded())

	c.recordRemoteError()
	assert.True(t, c.isDegraded())
}

func TestRecordRemoteErrorNoThreshold(t *testing.T) {
	// degradeThreshold = 0 means no degrade
	c := &cache{
		degradeThreshold: 0,
		metrics:          noopMetrics{},
		logger:           logger.DefaultLogger,
		stopChan:         make(chan struct{}),
	}

	for i := 0; i < 10; i++ {
		c.recordRemoteError()
	}
	assert.False(t, c.isDegraded())
}

func TestRecordRemoteSuccessExitsDegraded(t *testing.T) {
	c := &cache{
		degradeThreshold: 1,
		metrics:          noopMetrics{},
		logger:           logger.DefaultLogger,
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
		remoteStore:       remote,
		degradeRecovery:   10 * time.Millisecond,
		degradeStopRecov:  make(chan struct{}),
		stopChan:          make(chan struct{}),
		metrics:           noopMetrics{},
		logger:            logger.DefaultLogger,
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
		logger:           logger.DefaultLogger,
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
	remote.Put(context.Background(), "k", []byte("v"), 60)

	c := &cache{
		remoteStore: remote,
		stats:       newStats(),
		logger:      logger.DefaultLogger,
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
		logger:           logger.DefaultLogger,
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
func (f *failStore) Name() string      { return "fail" }
func (f *failStore) IsRemote() bool    { return true }

// --- noopMetrics usage from mem_store ---

func TestMemStoreEvictionCallsNoopMetrics(t *testing.T) {
	// noopMetrics.CacheEviction is called during eviction; should not panic
	s := newMemStore()
	s.maxItems = 2
	ctx := context.Background()

	s.Put(ctx, "a", []byte("1"), 60)
	s.Put(ctx, "b", []byte("2"), 60)
	s.Put(ctx, "c", []byte("3"), 60) // triggers eviction → calls noopMetrics.CacheEviction()

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
	remote.Put(context.Background(), "k", []byte("v"), 60)

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
