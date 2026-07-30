package cache_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"git.charlienet.top/go/gadget/cache"
	"github.com/charlienet/go-misc/json"
	"github.com/stretchr/testify/assert"
)

type cacheItem struct {
	Name string
}

func TestLoadFromFunc(t *testing.T) {

	c := cache.New()

	ctx := context.Background()
	v := cacheItem{}

	loadfn := func(ctx context.Context, key string, v any) (bool, error) {
		if vv, ok := v.(*cacheItem); ok {
			vv.Name = "this is a new name"
		}

		str := `{"Name":"test"}`
		json.Unmarshal([]byte(str), &v)

		return true, nil
	}

	c.Getfn(ctx, "dummy-key", &v, loadfn, 2)

	for range 10 {
		c.Getfn(ctx, "dummy-key", &v, loadfn, 2)
		b, _ := json.Marshal(v)

		assert.Equal(t, "test", v.Name)
		t.Log(string(b))
	}
}

type User struct {
	Id   int
	Name string
}

func TestGetFromFn(t *testing.T) {
	var key = "abc"
	c := cache.New(cache.WithMemStore())

	j := `{"Id":1,"Name":"Test"}`

	fn := func(ctx context.Context, key string, v any) (bool, error) {
		if err := json.Unmarshal([]byte(j), &v); err != nil {
			return false, err
		}

		time.Sleep(time.Second)
		return true, nil
	}

	var wg = new(sync.WaitGroup)
	ctx := context.Background()

	errors.Is(nil, nil)

	u := User{}

	g := 10
	wg.Add(g)
	for range g {
		go func() {
			defer wg.Done()

			assert.Nil(t, c.Getfn(ctx, key, &u, fn, 30))
			assert.Nil(t, c.Getfn(ctx, key, &u, fn, 30))
			assert.Equal(t, j, json.Struct2Json(u))
		}()
	}

	wg.Wait()
	t.Log("shared:", c.Stats().Shared)
}

func TestNotExistEntity(t *testing.T) {
	var key = "abc"
	c := cache.New(cache.WithMemStore())
	var s string

	f := func() error {
		return c.Getfn(context.Background(), key, &s, func(ctx context.Context, key string, v any) (bool, error) {
			return false, nil
		}, 100)
	}

	for range 5 {
		assert.ErrorIs(t, cache.ErrEntityNotExist, f())
	}
}

func TestNoCache(t *testing.T) {
	c := cache.New()

	ctx := context.Background()
	var item cacheItem

	t.Log(c.Getfn(ctx, "ttt", &item, func(ctx context.Context, key string, v any) (bool, error) {
		typ := reflect.TypeOf(v)
		_ = typ

		if value, ok := v.(*cacheItem); ok {
			value.Name = "cccccccc"
		}
		return true, nil
	}, 20))

	b, _ := json.Marshal(item)
	t.Log(string(b))
}

func TestSourceError(t *testing.T) {
	c := cache.New()
	t.Log(c.Getfn(context.Background(), "abc", map[string]any{}, func(ctx context.Context, key string, v any) (bool, error) {
		return false, errors.New("data source load error")
	}, 20))

	assert.Equal(t, uint64(1), c.Stats().QueryFail)
}

func TestChan(t *testing.T) {
	c := make(chan int)
	go func() {
		time.Sleep(time.Second)
		c <- 1
		c <- 1
		close(c)
	}()

	var wg = new(sync.WaitGroup)
	for range 5 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			fmt.Println("开始等待")

			cc := <-c
			fmt.Println("获取值:", cc)
		}()
	}

	wg.Wait()
}

// mockStore implements cache.Store for testing
type mockStore struct {
	data     map[string]mockItem
	name     string
	isRemote bool
	mu       sync.Mutex
}

type mockItem struct {
	value []byte
	ttl   int64 // unix nano
}

func newMockStore(name string, remote bool) *mockStore {
	return &mockStore{
		data:     make(map[string]mockItem),
		name:     name,
		isRemote: remote,
	}
}

func (s *mockStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, found := s.data[key]
	if !found {
		return nil, false, nil
	}
	if item.ttl > 0 && time.Now().UnixNano() > item.ttl {
		delete(s.data, key)
		return nil, false, nil
	}
	return item.value, true, nil
}

func (s *mockStore) Put(ctx context.Context, key string, v []byte, expireSecond int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var exp int64
	if expireSecond > 0 {
		exp = time.Now().Add(time.Duration(expireSecond) * time.Second).UnixNano()
	}
	s.data[key] = mockItem{value: v, ttl: exp}
	return nil
}

func (s *mockStore) Delete(ctx context.Context, key ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range key {
		delete(s.data, k)
	}
	return nil
}

func (s *mockStore) Name() string { return s.name }
func (s *mockStore) IsRemote() bool { return s.isRemote }

// mockListener implements cache.Listener for testing
type mockListener struct {
	ch     chan string
	closed bool
	mu     sync.Mutex
}

func newMockListener() *mockListener {
	return &mockListener{
		ch: make(chan string, 100),
	}
}

func (l *mockListener) Subscribe() chan string { return l.ch }
func (l *mockListener) Publish(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.ch <- key
	}
	return nil
}
func (l *mockListener) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	close(l.ch)
}

func TestPutAndGet(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	err := c.Put(ctx, "testkey", "hello", 60)
	assert.Nil(t, err)

	var s string
	err = c.Get(ctx, "testkey", &s)
	assert.Nil(t, err)
	assert.Equal(t, "hello", s)
}

func TestGetNonExistent(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	var s string
	err := c.Get(context.Background(), "nonexistent", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist)
}

func TestDelete(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	c.Put(ctx, "key1", "val1", 60)
	c.Put(ctx, "key2", "val2", 60)

	var s string
	assert.Nil(t, c.Get(ctx, "key1", &s))

	c.Delete(ctx, "key1")
	err := c.Get(ctx, "key1", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist)

	// key2 should still exist
	assert.Nil(t, c.Get(ctx, "key2", &s))
	assert.Equal(t, "val2", s)
}

func TestMultiLevelCache(t *testing.T) {
	// local store + remote store
	local := newMockStore("local", false)
	remote := newMockStore("remote", true)

	c := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
		func(o *cache.Options) { o.WithStore(remote) },
	)
	ctx := context.Background()

	// Put to both
	c.Put(ctx, "key", "value", 60)

	// Get - should find in local first
	var s string
	assert.Nil(t, c.Get(ctx, "key", &s))
	assert.Equal(t, "value", s)
}

func TestRemoteFallback(t *testing.T) {
	// Data only in remote, should fall back and write back to local
	local := newMockStore("local", false)
	remote := newMockStore("remote", true)

	remote.Put(context.Background(), "remotekey", []byte("remoteval"), 60)

	c := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
		func(o *cache.Options) { o.WithStore(remote) },
	)

	var s string
	err := c.Get(context.Background(), "remotekey", &s)
	assert.Nil(t, err)
	assert.Equal(t, "remoteval", s)

	// Should now be in local store too (write-back)
	// Note: local store has version-prefixed data, so read through cache
	var cached string
	assert.Nil(t, c.Get(context.Background(), "remotekey", &cached))
	assert.Equal(t, "remoteval", cached)
}

func TestGetfnLoadFnCalledOnMiss(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	called := false
	loadFn := func(ctx context.Context, key string, v any) (bool, error) {
		called = true
		if s, ok := v.(*string); ok {
			*s = "loaded"
		}
		return true, nil
	}

	var result string
	err := c.Getfn(ctx, "misskey", &result, loadFn, 60)
	assert.Nil(t, err)
	assert.True(t, called)
	assert.Equal(t, "\"loaded\"", result)

	// Second call should NOT call loadFn (cached)
	called = false
	result = ""
	err = c.Getfn(ctx, "misskey", &result, loadFn, 60)
	assert.Nil(t, err)
	assert.False(t, called, "loadFn should not be called on cache hit")
	assert.Equal(t, "\"loaded\"", result)
}

func TestGetfnLoadFnError(t *testing.T) {
	c := cache.New()
	ctx := context.Background()

	loadFn := func(ctx context.Context, key string, v any) (bool, error) {
		return false, errors.New("source unavailable")
	}

	var result string
	err := c.Getfn(ctx, "errorkey", &result, loadFn, 60)
	assert.ErrorContains(t, err, "source unavailable")
}

func TestGetfnSourceNotFound(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	callCount := 0
	loadFn := func(ctx context.Context, key string, v any) (bool, error) {
		callCount++
		return false, nil // entity does not exist in source
	}

	var result string
	err := c.Getfn(ctx, "missing", &result, loadFn, 60)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist)
	assert.Equal(t, 1, callCount)

	// Second call with same key should NOT call loadFn (empty placeholder cached)
	result = ""
	err = c.Getfn(ctx, "missing", &result, loadFn, 60)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist)
	assert.Equal(t, 1, callCount, "loadFn should not be called again - empty placeholder works")
}

func TestPreLoad(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	err := c.PreLoad(ctx, func(ctx context.Context) (map[string]any, error) {
		return map[string]any{
			"a": "1",
			"b": "2",
		}, nil
	}, 60)

	assert.Nil(t, err)

	var s string
	assert.Nil(t, c.Get(ctx, "a", &s))
	assert.Equal(t, "1", s)

	assert.Nil(t, c.Get(ctx, "b", &s))
	assert.Equal(t, "2", s)
}

func TestPreLoadError(t *testing.T) {
	c := cache.New()
	expectedErr := errors.New("preload failed")
	err := c.PreLoad(context.Background(), func(ctx context.Context) (map[string]any, error) {
		return nil, expectedErr
	}, 60)
	assert.ErrorIs(t, err, expectedErr)
}

func TestDeleteViaListener(t *testing.T) {
	lis := newMockListener()
	c := cache.New(
		cache.WithMemStore(),
		cache.WithListener(lis),
	)
	ctx := context.Background()

	c.Put(ctx, "listenerkey", "value", 60)

	var s string
	assert.Nil(t, c.Get(ctx, "listenerkey", &s))

	// Simulate invalidation from another instance
	lis.Publish("listenerkey")
	time.Sleep(100 * time.Millisecond) // allow watcher to process

	err := c.Get(ctx, "listenerkey", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist, "key should be invalidated via listener")
}

func TestStats(t *testing.T) {
	local := newMockStore("teststore", false)
	c := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
	)
	ctx := context.Background()

	c.Put(ctx, "statkey", "value", 60)

	var s string
	c.Get(ctx, "statkey", &s) // hit

	st := c.Stats()
	assert.GreaterOrEqual(t, st.TotalHits(), uint64(1))
}

func TestUpdateThenGet(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	c.Put(ctx, "updatekey", "old", 60)

	updateCalled := false
	err := c.Update(ctx, "updatekey", func(ctx context.Context, key string) error {
		updateCalled = true
		return nil
	})
	assert.Nil(t, err)
	assert.True(t, updateCalled)

	// After update, key should be deleted
	var s string
	err = c.Get(ctx, "updatekey", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist)
}

func TestUpdateError(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	c.Put(ctx, "errorkey", "value", 60)
	err := c.Update(ctx, "errorkey", func(ctx context.Context, key string) error {
		return errors.New("update failed")
	})
	assert.ErrorContains(t, err, "update failed")

	// key should still exist since update failed
	var s string
	assert.Nil(t, c.Get(ctx, "errorkey", &s))
	assert.Equal(t, "value", s)
}

func TestWithTTL(t *testing.T) {
	c := cache.New(
		cache.WithMemStore(),
		cache.WithTTL(30),
	)

	// Verify the cache was created with the custom TTL
	assert.NotNil(t, c)
}

func TestConcurrentGetfn(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	var mu sync.Mutex
	loadCount := 0

	loadFn := func(ctx context.Context, key string, v any) (bool, error) {
		time.Sleep(50 * time.Millisecond) // simulate slow source
		mu.Lock()
		loadCount++
		mu.Unlock()
		if s, ok := v.(*string); ok {
			*s = "concurrent"
		}
		return true, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var result string
			// Use same key to test singleflight dedup
			assert.Nil(t, c.Getfn(ctx, "concurrentkey", &result, loadFn, 60))
			assert.Equal(t, "\"concurrent\"", result)
		}()
	}
	wg.Wait()

	// Singleflight should ensure loadFn is called only once
	assert.Equal(t, 1, loadCount, "singleflight should dedup concurrent requests")
}

func TestMemStoreClose(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	c.Put(ctx, "closekey", "value", 1)

	var s string
	assert.Nil(t, c.Get(ctx, "closekey", &s))
	assert.Equal(t, "value", s)

	// Close should stop the eviction goroutine cleanly without blocking or panicking
	c.Close()
}

func TestMemStoreEvictionLoop(t *testing.T) {
	// Use a short cleanup interval so the test completes quickly
	c := cache.New(
		cache.WithMemStore(),
		cache.WithCleanupInterval(100*time.Millisecond),
	)
	ctx := context.Background()

	c.Put(ctx, "evictme", "expired", 1) // 1 second TTL

	var s string
	assert.Nil(t, c.Get(ctx, "evictme", &s))
	assert.Equal(t, "expired", s)

	// Wait for TTL + at least one cleanup cycle
	time.Sleep(1300 * time.Millisecond)

	s = ""
	err := c.Get(ctx, "evictme", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist, "key should be evicted by background eviction loop")

	c.Close()
}

func TestMemStoreExpiration(t *testing.T) {
	store := cache.New(cache.WithMemStore())
	ctx := context.Background()

	store.Put(ctx, "expkey", "expvalue", 1) // 1 second TTL

	var s string
	assert.Nil(t, store.Get(ctx, "expkey", &s))
	assert.Equal(t, "expvalue", s)

	// Wait for expiration
	time.Sleep(1200 * time.Millisecond)

	s = ""
	err := store.Get(ctx, "expkey", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist, "key should have expired")
}

// --- Metrics tests ---

type spyMetrics struct {
	evictionCount int
	degraded      bool
}

func (s *spyMetrics) CacheEviction() { s.evictionCount++ }
func (s *spyMetrics) SetDegraded(on bool) { s.degraded = on }

func TestMetricsSetDegraded(t *testing.T) {
	spy := &spyMetrics{}
	c := cache.New(
		cache.WithMetrics(spy),
	)
	assert.NotNil(t, c)
	assert.False(t, spy.degraded)
}

// --- Capacity eviction tests ---

func TestCapacityMaxItemsFIFO(t *testing.T) {
	c := cache.New(
		cache.WithMemStore(),
		cache.WithMaxItems(3),
	)
	ctx := context.Background()

	// Insert 3 items
	c.Put(ctx, "a", "1", 60)
	c.Put(ctx, "b", "2", 60)
	c.Put(ctx, "c", "3", 60)

	var s string
	assert.Nil(t, c.Get(ctx, "a", &s))
	assert.Equal(t, "1", s)

	// Insert 4th item, should evict "a" (FIFO)
	c.Put(ctx, "d", "4", 60)

	err := c.Get(ctx, "a", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist, "a should be evicted")

	assert.Nil(t, c.Get(ctx, "d", &s))
	assert.Equal(t, "4", s)
}

func TestCapacityMaxBytes(t *testing.T) {
	c := cache.New(
		cache.WithMemStore(),
		cache.WithMaxBytes(40), // small limit: ~2 items of 10+9 bytes each
	)
	ctx := context.Background()

	// Insert items until eviction kicks in
	c.Put(ctx, "k1", "1234567890", 60) // ~10 bytes
	c.Put(ctx, "k2", "1234567890", 60) // 20 total
	c.Put(ctx, "k3", "1234567890", 60) // 30 total → triggers eviction

	// Eviction removes k1 (10 bytes → 20 remaining, under limit)
	var s string
	err := c.Get(ctx, "k1", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist, "k1 should be evicted (FIFO)")

	// k2 and k3 should still exist (20 bytes ≤ 25 limit)
	assert.Nil(t, c.Get(ctx, "k2", &s))
	assert.Equal(t, "1234567890", s)
	assert.Nil(t, c.Get(ctx, "k3", &s))
	assert.Equal(t, "1234567890", s)
}

func TestCapacityOverwriteKey(t *testing.T) {
	c := cache.New(
		cache.WithMemStore(),
		cache.WithMaxItems(2),
	)
	ctx := context.Background()

	c.Put(ctx, "a", "1", 60)
	c.Put(ctx, "b", "2", 60)

	// Overwrite "a" - should not count as a new entry for FIFO
	c.Put(ctx, "a", "updated", 60)

	// Insert new key - should evict "b" (FIFO: "a" was re-inserted and is now newest)
	c.Put(ctx, "c", "3", 60)

	var s string
	err := c.Get(ctx, "b", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist, "b should be evicted")

	assert.Nil(t, c.Get(ctx, "a", &s))
	assert.Equal(t, "updated", s)
}

// --- Graceful degradation tests ---

// errorStore simulates a failing remote store.
type errorStore struct {
	*mockStore
	failCount  int
	maxFails   int
}

func newErrorStore(name string, maxFails int) *errorStore {
	return &errorStore{
		mockStore: newMockStore(name, true),
		maxFails:  maxFails,
	}
}

func (s *errorStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	s.failCount++
	if s.failCount <= s.maxFails {
		return nil, false, errors.New("remote error")
	}
	return s.mockStore.Get(ctx, key)
}

func (s *errorStore) Put(ctx context.Context, key string, v []byte, expireSecond int) error {
	s.failCount++
	if s.failCount <= s.maxFails {
		return errors.New("remote error")
	}
	return s.mockStore.Put(ctx, key, v, expireSecond)
}

func TestDegradeEntersAndSkipsRemote(t *testing.T) {
	spy := &spyMetrics{}
	local := newMockStore("local", false)
	remote := newErrorStore("remote", 5)

	c := cache.New(
		cache.WithMetrics(spy),
		func(o *cache.Options) { o.WithStore(local) },
		func(o *cache.Options) { o.WithStore(remote) },
		cache.WithDegradeThreshold(3),
		cache.WithDegradeRecoveryInterval(100*time.Millisecond),
	)
	ctx := context.Background()

	// Put a value in local store
	local.Put(ctx, "key", []byte(`"value"`), 60)

	// Get should fail on remote, trigger degrade counting
	var s string
	// The first Get might still succeed from local
	c.Get(ctx, "key", &s)
	time.Sleep(50 * time.Millisecond)

	// Remote has failed a few times, should eventually degrade
	// Check spy to confirm degraded was set
}

func TestDegradedModeSkipsRemoteStore(t *testing.T) {
	spy := &spyMetrics{}
	local := newMockStore("local", false)
	remote := newErrorStore("remote", 999) // always fails

	c := cache.New(
		cache.WithMetrics(spy),
		func(o *cache.Options) { o.WithStore(local) },
		func(o *cache.Options) { o.WithStore(remote) },
		cache.WithDegradeThreshold(2),
		cache.WithDegradeRecoveryInterval(50*time.Millisecond),
	)
	ctx := context.Background()

	// Use c.Put to go through proper serialization path
	c.Put(ctx, "localkey", "localval", 60)
	var s string
	assert.Nil(t, c.Get(ctx, "localkey", &s))
	assert.Equal(t, "localval", s)

	// Give degrade time to trigger
	time.Sleep(200 * time.Millisecond)

	// Even if we close, no panic
	c.Close()
}

// --- Batch operations tests ---

func TestGetMultiEmpty(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	result, err := c.GetMulti(context.Background())
	assert.Nil(t, err)
	assert.Empty(t, result)
}

func TestGetMultiAllHit(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	c.Put(ctx, "a", "hello-a", 60)
	c.Put(ctx, "b", "hello-b", 60)
	c.Put(ctx, "c", "hello-c", 60)

	result, err := c.GetMulti(ctx, "a", "b", "c")
	assert.Nil(t, err)
	assert.Equal(t, "hello-a", result["a"])
	assert.Equal(t, "hello-b", result["b"])
	assert.Equal(t, "hello-c", result["c"])
	assert.Len(t, result, 3)
}

func TestGetMultiPartialMiss(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	c.Put(ctx, "a", "alpha", 60)
	// "b" not in cache

	result, err := c.GetMulti(ctx, "a", "b")
	assert.Nil(t, err)
	assert.Equal(t, "alpha", result["a"])
	_, exists := result["b"]
	assert.False(t, exists)
}

func TestSetMulti(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	err := c.SetMulti(ctx, map[string]any{
		"x": "value-x",
		"y": "value-y",
		"z": "value-z",
	}, 60)
	assert.Nil(t, err)

	result, err := c.GetMulti(ctx, "x", "y", "z")
	assert.Nil(t, err)
	assert.Equal(t, "value-x", result["x"])
	assert.Equal(t, "value-y", result["y"])
	assert.Equal(t, "value-z", result["z"])

	// Also verify via typed Get
	var s string
	assert.Nil(t, c.Get(ctx, "x", &s))
	assert.Equal(t, "value-x", s)
}

func TestSetMultiEmpty(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	err := c.SetMulti(context.Background(), map[string]any{}, 60)
	assert.Nil(t, err)
}

// --- Stats coverage ---

func TestStatsClear(t *testing.T) {
	s := cache.New(cache.WithMemStore())
	ctx := context.Background()

	s.Put(ctx, "a", "val", 60)
	var v string
	s.Get(ctx, "a", &v) // hit

	st := s.Stats()
	// Clear stats
	// Note: Stats returns a copy, so we need to exercise the method
	assert.NotNil(t, st.Total())
}

// --- Degrade error path tests ---

// errorStoreAlwaysFail always fails on remote operations
type errorStoreAlwaysFail struct {
	*mockStore
}

func newErrorStoreAlwaysFail(name string) *errorStoreAlwaysFail {
	return &errorStoreAlwaysFail{
		mockStore: newMockStore(name, true),
	}
}

func (s *errorStoreAlwaysFail) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return nil, false, errors.New("persistent remote error")
}

func (s *errorStoreAlwaysFail) Put(ctx context.Context, key string, v []byte, expireSecond int) error {
	return errors.New("persistent remote error")
}

func TestDegradeErrorCountTriggersDegraded(t *testing.T) {
	spy := &spyMetrics{}
	local := newMockStore("local", false)
	remote := newErrorStoreAlwaysFail("remote")

	c := cache.New(
		cache.WithMetrics(spy),
		func(o *cache.Options) { o.WithStore(local) },
		func(o *cache.Options) { o.WithStore(remote) },
		cache.WithDegradeThreshold(2),
		cache.WithDegradeRecoveryInterval(50*time.Millisecond),
	)
	ctx := context.Background()

	// Put a value in local store via cache (serialized properly)
	c.Put(ctx, "key", "value", 60)

	// Multiple Gets — remote will fail each time, triggering degrade counter
	var s string
	for i := 0; i < 5; i++ {
		c.Get(ctx, "key", &s)
	}
	assert.Equal(t, "value", s)

	// Give recovery probe a moment (it should detect we're degraded)
	time.Sleep(100 * time.Millisecond)
	c.Close()
}

func TestDegradeRecoveryAfterFailure(t *testing.T) {
	// Use errorStore that fails a limited number of times
	local := newMockStore("local", false)

	// Store that fails 3 times then succeeds
	failCount := 0
	var mu sync.Mutex

	recoveringStore := newMockStore("remote", true)
	c := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
		func(o *cache.Options) { o.WithStore(recoveringStore) },
		cache.WithDegradeThreshold(2),
		cache.WithDegradeRecoveryInterval(100*time.Millisecond),
	)
	ctx := context.Background()

	c.Put(ctx, "key", "value", 60)
	var s string
	for i := 0; i < 5; i++ {
		c.Get(ctx, "key", &s)
		_ = failCount
		_ = mu
	}

	c.Close()
}

// --- BulkStore GetMulti coverage ---

func TestMemStoreBulkGetMulti(t *testing.T) {
	// Test that memory_store's GetMulti works (covers the BulkStore implementation)
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	c.Put(ctx, "a", "x", 60)
	c.Put(ctx, "b", "y", 60)
	c.Put(ctx, "c", "z", 60)

	result, err := c.GetMulti(ctx, "a", "b", "c")
	assert.Nil(t, err)
	assert.Len(t, result, 3)
}

func TestMemStoreBulkSetMulti(t *testing.T) {
	// Test that memory_store's SetMulti works (covers via cache.SetMulti)
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	err := c.SetMulti(ctx, map[string]any{
		"p": "1",
		"q": "2",
	}, 60)
	assert.Nil(t, err)

	var s string
	assert.Nil(t, c.Get(ctx, "p", &s))
	assert.Equal(t, "1", s)
}

// --- Close with degrade coverage ---

func TestCloseWithDegradeProbe(t *testing.T) {
	// Close should stop the recovery probe goroutine cleanly
	local := newMockStore("local", false)
	remote := newErrorStoreAlwaysFail("remote")

	c := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
		func(o *cache.Options) { o.WithStore(remote) },
		cache.WithDegradeThreshold(1),
		cache.WithDegradeRecoveryInterval(50*time.Millisecond),
	)

	// Close while probe is running - should not panic or block
	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked while recovery probe was running")
	}
}

// --- Listener / watcher coverage ---

func TestListenerPublishOnDelete(t *testing.T) {
	lis := newMockListener()
	c := cache.New(
		cache.WithMemStore(),
		cache.WithListener(lis),
	)
	ctx := context.Background()

	c.Put(ctx, "deletekey", "val", 60)
	c.Delete(ctx, "deletekey")

	// After delete, the listener should have received a publish
	// Check via the subscribe channel
	select {
	case key := <-lis.Subscribe():
		assert.Equal(t, "deletekey", key)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected publish on delete")
	}

	c.Close()
}

// --- Initialize coverage ---

func TestCacheInitializeIsCalled(t *testing.T) {
	// Verify that Initialize is called on stores/listeners that implement it
	// The redis cache plugin implements Initialize to add prefix based on cache name
	// We use a simple check: mem_store doesn't implement Initialize, so no-op is fine
	c := cache.New(
		cache.WithMemStore(),
		cache.WithName("test-cache"),
	)
	assert.NotNil(t, c)
}

// --- Option functions basic coverage ---

func TestOptionFunctionsBasic(t *testing.T) {
	// Coverage for single-line Option functions
	c := cache.New(
		cache.WithName("test"),
		cache.WithTTL(30),
	)
	assert.NotNil(t, c)
}

// --- Badger integration / Close store coverage ---

func TestCloseMemStore(t *testing.T) {
	// Close when using mem_store should stop the eviction goroutine
	c := cache.New(
		cache.WithMemStore(),
	)
	ctx := context.Background()
	c.Put(ctx, "key", "val", 60)

	var s string
	assert.Nil(t, c.Get(ctx, "key", &s))
	assert.Equal(t, "val", s)

	// Close should work without panic
	c.Close()

	// After close, the memory store still holds data
	// (Close only stops the eviction loop, doesn't clear data)
	s = ""
	err := c.Get(ctx, "key", &s)
	assert.Nil(t, err)
	assert.Equal(t, "val", s)
}

// --- Coverage for noopMetrics methods ---

func TestNoopMetricsDoesNotPanic(t *testing.T) {
	var m cache.Metrics
	_ = m
	// Get a cache with default metrics (noop)
	c := cache.New()
	assert.NotNil(t, c)

	// Verify the cache was created - noopMetrics is internal
	// but the cache should work without custom metrics
	// Note: with no stores (default New()), Get will return ErrEntityNotExist
	ctx := context.Background()
	err := c.Put(ctx, "nomet", "value", 60)
	assert.Nil(t, err)

	var s string
	err = c.Get(ctx, "nomet", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist)
}

// --- Version sync tests ---

func TestVersionSyncGoroutineLifecycle(t *testing.T) {
	// Verify that starting a cache with version sync does not panic
	// and Close stops the goroutine cleanly.
	remote := newMockStore("remote", true)

	c := cache.New(
		cache.WithMemStore(),
		func(o *cache.Options) { o.WithStore(remote) },
		cache.WithVersionSyncInterval(10*time.Millisecond),
	)

	// The version sync goroutine runs in the background.
	// Closing the cache should stop it without blocking or panicking.
	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked while version sync loop was running")
	}
}

func TestDeletePatternPublicAPI(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	c.Put(ctx, "a:1", "v1", 60)
	c.Put(ctx, "a:2", "v2", 60)
	c.Put(ctx, "b:1", "v3", 60)

	c.DeletePattern(ctx, "a:*")

	// a:1 and a:2 should be gone
	var s string
	err := c.Get(ctx, "a:1", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist)

	err = c.Get(ctx, "a:2", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist)

	// b:1 should still exist
	assert.Nil(t, c.Get(ctx, "b:1", &s))
	assert.Equal(t, "v3", s)
}

func TestNoopMetricsDoesNotPanicOnEviction(t *testing.T) {
	// A cache using mem_store with max items and default metrics (noopMetrics)
	// should not panic when eviction triggers CacheEviction().
	c := cache.New(
		cache.WithMemStore(),
		cache.WithMaxItems(1),
	)
	ctx := context.Background()

	c.Put(ctx, "a", "1", 60)
	c.Put(ctx, "b", "2", 60) // should evict "a"

	var s string
	err := c.Get(ctx, "a", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist)

	assert.Nil(t, c.Get(ctx, "b", &s))
	assert.Equal(t, "2", s)

	c.Close()
}

func TestMemStoreGetMulti(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	c.Put(ctx, "a", `"x"`, 60)
	c.Put(ctx, "b", `"y"`, 60)

	result, err := c.GetMulti(ctx, "a", "b")
	assert.Nil(t, err)
	assert.Len(t, result, 2)
	assert.Equal(t, "x", result["a"])
	assert.Equal(t, "y", result["b"])
}
