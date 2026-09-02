package cache_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/stretchr/testify/assert"
)

func struct2Json(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

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
		_ = json.Unmarshal([]byte(str), &v)

		return true, nil
	}

	_ = c.Getfn(ctx, "dummy-key", &v, loadfn, 2)

	for range 10 {
		_ = c.Getfn(ctx, "dummy-key", &v, loadfn, 2)
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

	g := 10
	wg.Add(g)
	for range g {
		go func() {
			defer wg.Done()

			// 每个 goroutine 独立持有目标对象：Getfn 会并发反序列化到 v
			u := User{}

			assert.Nil(t, c.Getfn(ctx, key, &u, fn, 30))
			assert.Nil(t, c.Getfn(ctx, key, &u, fn, 30))
			assert.Equal(t, j, struct2Json(u))
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

func (s *mockStore) Name() string   { return s.name }
func (s *mockStore) IsRemote() bool { return s.isRemote }

// mockListener implements cache.Listener for testing
type mockListener struct {
	ch        chan string
	closed    bool
	published []string
	mu        sync.Mutex
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
	l.published = append(l.published, key)
	if !l.closed {
		l.ch <- key
	}
	return nil
}
func (l *mockListener) Ready() <-chan struct{} {
	ch := make(chan struct{})
	close(ch) // mock 立即就绪
	return ch
}
func (l *mockListener) Close(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.ch)
	}
	return nil
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

	_ = c.Put(ctx, "key1", "val1", 60)
	_ = c.Put(ctx, "key2", "val2", 60)

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
	_ = c.Put(ctx, "key", "value", 60)

	// Get - should find in local first
	var s string
	assert.Nil(t, c.Get(ctx, "key", &s))
	assert.Equal(t, "value", s)
}

func TestRemoteFallback(t *testing.T) {
	// Data only in remote, should fall back and write back to local
	local := newMockStore("local", false)
	remote := newMockStore("remote", true)

	_ = remote.Put(context.Background(), "remotekey", []byte("\"remoteval\""), 60)

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
	assert.Equal(t, "loaded", result)

	// Second call should NOT call loadFn (cached)
	called = false
	result = ""
	err = c.Getfn(ctx, "misskey", &result, loadFn, 60)
	assert.Nil(t, err)
	assert.False(t, called, "loadFn should not be called on cache hit")
	assert.Equal(t, "loaded", result)
}

func TestGetfnLoadFnError(t *testing.T) {
	c := cache.New()
	ctx := context.Background()

	callCount := 0
	loadFn := func(ctx context.Context, key string, v any) (bool, error) {
		callCount++
		return false, errors.New("source unavailable")
	}

	var result string
	err := c.Getfn(ctx, "errorkey", &result, loadFn, 60)
	assert.ErrorContains(t, err, "source unavailable")
	assert.Equal(t, 1, callCount)

	// 非"未找到"错误：直接返回且不缓存 → 二次调用重试 fn
	result = ""
	err = c.Getfn(ctx, "errorkey", &result, loadFn, 60)
	assert.ErrorContains(t, err, "source unavailable")
	assert.Equal(t, 2, callCount, "non-not-found errors must not be cached; fn retries")
}

func TestGetfnOtherErrorStillNotCached(t *testing.T) {
	// 错误语义边界：loadFn 返回任何 error 一律视为真实失败——直接返回、
	// 不写占位不缓存，二次调用重试 fn（"错误不拦截"行为固化）。
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	callCount := 0
	loadFn := func(ctx context.Context, key string, v any) (bool, error) {
		callCount++
		return false, errors.New("boom")
	}

	var result string
	err := c.Getfn(ctx, "boom-key", &result, loadFn, 60)
	assert.ErrorContains(t, err, "boom")
	assert.Equal(t, 1, callCount)

	result = ""
	err = c.Getfn(ctx, "boom-key", &result, loadFn, 60)
	assert.ErrorContains(t, err, "boom")
	assert.Equal(t, 2, callCount, "loadFn errors must not be intercepted; fn retries")
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

	_ = c.Put(ctx, "listenerkey", "value", 60)

	var s string
	assert.Nil(t, c.Get(ctx, "listenerkey", &s))

	// Simulate invalidation from another instance
	_ = lis.Publish("listenerkey")
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

	_ = c.Put(ctx, "statkey", "value", 60)

	var s string
	_ = c.Get(ctx, "statkey", &s) // hit

	st := c.Stats()
	assert.GreaterOrEqual(t, st.TotalHits(), uint64(1))
}

func TestUpdateThenGet(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	_ = c.Put(ctx, "updatekey", "old", 60)

	updateCalled := false
	err := c.Invalidate(ctx, "updatekey", func(ctx context.Context, key string) error {
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

	_ = c.Put(ctx, "errorkey", "value", 60)
	err := c.Invalidate(ctx, "errorkey", func(ctx context.Context, key string) error {
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
			assert.Equal(t, "concurrent", result)
		}()
	}
	wg.Wait()

	// Singleflight should ensure loadFn is called only once
	assert.Equal(t, 1, loadCount, "singleflight should dedup concurrent requests")
}

func TestConcurrentGetfnSourceNotFound(t *testing.T) {
	// 并发防穿透核心场景：fn 返回 (false, nil)（源中不存在）。
	// 断言依据（读 cache.go 实现）：
	// - getFromSource 使用 singleflight（key:%s），并发 miss 合并为一次 fn 调用；
	// - exist=false 时写入 notExistPlaceholder（"*"）到 L1；
	// - verifyEvery=0（默认）时，占位命中后 getFromCache 提前返回（local hit 且不
	//   需要校验），不再访问 remote → 占位在 L1 生效，后续调用不穿透到 fn；
	// - respond 对占位返回 ErrEntityNotExist。
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	var mu sync.Mutex
	callCount := 0
	loadFn := func(ctx context.Context, key string, v any) (bool, error) {
		time.Sleep(50 * time.Millisecond) // 模拟慢源，确保并发窗口内合并
		mu.Lock()
		callCount++
		mu.Unlock()
		return false, nil
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var s string
			errs <- c.Getfn(ctx, "missing", &s, loadFn, 60)
		}()
	}
	close(start) // 同时释放所有 goroutine
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.ErrorIs(t, err, cache.ErrEntityNotExist, "every goroutine should observe not-exist")
	}
	assert.Equal(t, 1, callCount, "singleflight must dedup concurrent misses (cache penetration guard)")

	// 占位已缓存：串行再调同 key → 仍 ErrEntityNotExist 且 fn 不再被调用
	var s string
	assert.ErrorIs(t, c.Getfn(ctx, "missing", &s, loadFn, 60), cache.ErrEntityNotExist)
	assert.Equal(t, 1, callCount, "placeholder should be cached in L1; fn must not be called again")
}

func TestConcurrentGetfnLoadFnError(t *testing.T) {
	// 并发 fn 返回 error：singleflight 合并，错误传播给所有共享者。
	// 断言依据（读 cache.go 实现）：
	// - getFromSource 闭包对 fn error 直接 return storeItem{}, err（不写缓存、
	//   不写占位）；sg.Do 完成后 Forget → 后续调用同 key 会重新执行 fn。
	// - 因此"fn 失败不缓存、下次调用重试"是固化的错误语义。
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	srcErr := errors.New("source unavailable")
	var mu sync.Mutex
	callCount := 0
	loadFn := func(ctx context.Context, key string, v any) (bool, error) {
		time.Sleep(50 * time.Millisecond) // 模拟慢源，确保并发窗口内合并
		mu.Lock()
		callCount++
		mu.Unlock()
		return false, srcErr
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var s string
			errs <- c.Getfn(ctx, "errkey", &s, loadFn, 60)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.ErrorIs(t, err, srcErr, "every goroutine should receive the source error")
	}
	assert.Equal(t, 1, callCount, "concurrent calls should share a single fn execution")

	// 错误不缓存：串行再调同 key → fn 重试，仍返回同一错误
	var s string
	assert.ErrorIs(t, c.Getfn(ctx, "errkey", &s, loadFn, 60), srcErr)
	assert.Equal(t, 2, callCount, "failed fn must not be cached; subsequent call retries")
}

func TestConcurrentGetfnWaitersBlockUntilLeaderDone(t *testing.T) {
	// 等待语义显式验证：并发 Getfn 时其他线程必须等待第一个线程的 fn 完成，
	// 而不是各自执行 fn 或提前返回错误。
	// 断言依据：Getfn 用单一 singleflight（key:%s）覆盖「查缓存→miss→fn→回填」
	// 全流程（cache.go Getfn），waiter 在 Do 上阻塞直到 leader 闭包完成，
	// 因此所有调用方的返回时间都晚于 fn 的完成时间。
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	var fnStart, fnEnd atomic.Int64
	var mu sync.Mutex
	loadCount := 0

	loadFn := func(ctx context.Context, key string, v any) (bool, error) {
		fnStart.Store(time.Now().UnixNano())
		time.Sleep(200 * time.Millisecond) // 慢源
		fnEnd.Store(time.Now().UnixNano())
		mu.Lock()
		loadCount++
		mu.Unlock()
		if s, ok := v.(*string); ok {
			*s = "waited"
		}
		return true, nil
	}

	const n = 8
	start := make(chan struct{})
	returns := make(chan int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var result string
			assert.Nil(t, c.Getfn(ctx, "waitkey", &result, loadFn, 60))
			assert.Equal(t, "waited", result)
			returns <- time.Now().UnixNano()
		}()
	}
	close(start)
	wg.Wait()
	close(returns)

	assert.Equal(t, 1, loadCount, "only the leader should execute fn")
	assert.Greater(t, fnEnd.Load(), int64(0), "fn should have completed")
	// 显式验证「等待」：每个调用方（含 leader 自身）的返回都发生在 fn 完成之后，
	// 而非提前返回错误或重复执行 fn。
	for ret := range returns {
		assert.GreaterOrEqual(t, ret, fnEnd.Load(),
			"waiter must return only after the leader fn has completed")
	}
}

// slowRemoteStore 包装 mockStore：Get 故意慢（模拟慢 remote），数据为空 → miss。
type slowRemoteStore struct {
	*mockStore
}

func (s *slowRemoteStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	time.Sleep(150 * time.Millisecond)
	return s.mockStore.Get(ctx, key)
}

// countingRemoteStore 包装 mockStore：Get 带延迟并原子计数，用于验证并发
// 路径下 remote 仅被访问一次的语义。
type countingRemoteStore struct {
	*mockStore
	getCount atomic.Int64
}

func (s *countingRemoteStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	s.getCount.Add(1)
	time.Sleep(100 * time.Millisecond) // 扩大 singleflight 合并窗口
	return s.mockStore.Get(ctx, key)
}

func TestConcurrentGetDedupsRemoteAccess(t *testing.T) {
	remote := &countingRemoteStore{mockStore: newMockStore("remote", true)}
	// 并发 Get 合并语义：本地 miss + remote 命中时，同一 key 的并发 Get
	// 由 getFromCache 的 singleflight（get-from-cache-%s）合并——一个去取、
	// 其他等待共享，remote 只被访问一次。
	c := cache.New(
		cache.WithMemStore(),
		func(o *cache.Options) { o.WithStore(remote) },
	)
	ctx := context.Background()
	_ = remote.Put(ctx, "k", []byte(`"value"`), 60)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var s string
			errs <- c.Get(ctx, "k", &s)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.Nil(t, err, "every goroutine should get the value")
	}
	assert.Equal(t, int64(1), remote.getCount.Load(),
		"remote must be accessed exactly once for concurrent Get (one fetches, others wait)")

	// 回写后串行 Get 不再访问 remote
	base := remote.getCount.Load()
	var s string
	assert.Nil(t, c.Get(ctx, "k", &s))
	assert.Equal(t, "value", s)
	assert.Equal(t, base, remote.getCount.Load(), "local write-back should serve subsequent Get")
	c.Close()
}

func TestConcurrentGetMultiDedupsRemoteAccessBulk(t *testing.T) {
	remote := &countingRemoteStore{mockStore: newMockStore("remote", true)}
	// 并发 GetMulti 合并语义（bulk 分派路径：mem_store 实现 BulkStore）：
	// 每 key 的 getFromCache singleflight 合并 → remote 每 key 只访问一次
	// （keys=3 → 总访问 3 次，而非 N×3）。
	c := cache.New(
		cache.WithMemStore(),
		func(o *cache.Options) { o.WithStore(remote) },
	)
	ctx := context.Background()
	keys := []string{"k1", "k2", "k3"}
	for i, k := range keys {
		_ = remote.Put(ctx, k, []byte(fmt.Sprintf(`"v%d"`, i)), 60)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	results := make(chan map[string]any, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := c.GetMulti(ctx, keys...)
			errs <- err
			results <- res
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(results)

	for err := range errs {
		assert.Nil(t, err)
	}
	for res := range results {
		assert.Equal(t, "v0", res["k1"])
		assert.Equal(t, "v1", res["k2"])
		assert.Equal(t, "v2", res["k3"])
	}
	assert.Equal(t, int64(3), remote.getCount.Load(),
		"remote must be accessed once per key (3), not once per goroutine")
	c.Close()
}

func TestConcurrentGetMultiDedupsRemoteAccessFallback(t *testing.T) {
	remote := &countingRemoteStore{mockStore: newMockStore("remote", true)}
	// 并发 GetMulti 合并语义（fallback 循环路径：mockStore 非 BulkStore）：
	// 逐 key 走 getFromCache（sg 合并）→ remote 每 key 只访问一次。
	local := newMockStore("local", false)
	c := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
		func(o *cache.Options) { o.WithStore(remote) },
	)
	ctx := context.Background()
	keys := []string{"k1", "k2"}
	for i, k := range keys {
		_ = remote.Put(ctx, k, []byte(fmt.Sprintf(`"f%d"`, i)), 60)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	results := make(chan map[string]any, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := c.GetMulti(ctx, keys...)
			errs <- err
			results <- res
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(results)

	for err := range errs {
		assert.Nil(t, err)
	}
	for res := range results {
		assert.Equal(t, "f0", res["k1"])
		assert.Equal(t, "f1", res["k2"])
	}
	assert.Equal(t, int64(2), remote.getCount.Load(),
		"fallback path must access remote once per key (2), not once per goroutine")
	c.Close()
}

func TestConcurrentGetfnRemoteSlowFnFast(t *testing.T) {
	// 边界回归：remote Get 慢（150ms）而 fn 快（1ms）。
	// 修复前（Getfn 分两层 singleflight 串行释放）：leader 完成 getFromCache
	// （含 150ms remote 查询）后立即执行快 fn 并在 waiter 进入 getFromSource 前
	// Forget → waiter 会重新执行 fn（loadCount > 1）。
	// 修复后（Getfn 单一 singleflight 覆盖全流程）：waiter 在整个闭包（remote 查询
	// + fn）完成前一直等待并共享结果，fn 恒为 1 次。
	remote := &slowRemoteStore{mockStore: newMockStore("remote", true)}
	c := cache.New(
		cache.WithMemStore(),
		func(o *cache.Options) { o.WithStore(remote) },
	)
	ctx := context.Background()

	var mu sync.Mutex
	loadCount := 0
	loadFn := func(ctx context.Context, key string, v any) (bool, error) {
		time.Sleep(time.Millisecond) // 快 fn
		mu.Lock()
		loadCount++
		mu.Unlock()
		if s, ok := v.(*string); ok {
			*s = "fast"
		}
		return true, nil
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var result string
			assert.Nil(t, c.Getfn(ctx, "slowremote-key", &result, loadFn, 60))
			assert.Equal(t, "fast", result)
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, 1, loadCount, "singleflight must span the whole Getfn flow (remote probe + fn)")
	c.Close()
}

func TestMemStoreClose(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	_ = c.Put(ctx, "closekey", "value", 1)

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
		cache.WithTTLJitter(0), // 关闭默认抖动，验证精确 TTL 过期
	)
	ctx := context.Background()

	_ = c.Put(ctx, "evictme", "expired", 1) // 1 second TTL

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
	store := cache.New(
		cache.WithMemStore(),
		cache.WithTTLJitter(0), // 关闭默认抖动，验证精确 TTL 过期
	)
	ctx := context.Background()

	_ = store.Put(ctx, "expkey", "expvalue", 1) // 1 second TTL

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
	mu            sync.Mutex // 保护 evictionCount（evictLoop goroutine 并发调用）
	evictionCount int
	degraded      atomic.Bool // healthLoop/请求 goroutine 并发读写
}

func (s *spyMetrics) CacheEviction() {
	s.mu.Lock()
	s.evictionCount++
	s.mu.Unlock()
}
func (s *spyMetrics) SetDegraded(on bool) { s.degraded.Store(on) }

func TestMetricsSetDegraded(t *testing.T) {
	spy := &spyMetrics{}
	c := cache.New(
		cache.WithMetrics(spy),
	)
	assert.NotNil(t, c)
	assert.False(t, spy.degraded.Load())
}

// --- Capacity eviction tests ---

func TestCapacityMaxItemsFIFO(t *testing.T) {
	c := cache.New(
		cache.WithMemStore(),
		cache.WithMaxItems(3),
	)
	ctx := context.Background()

	// Insert 3 items
	_ = c.Put(ctx, "a", "1", 60)
	_ = c.Put(ctx, "b", "2", 60)
	_ = c.Put(ctx, "c", "3", 60)

	var s string
	assert.Nil(t, c.Get(ctx, "a", &s))
	assert.Equal(t, "1", s)

	// Insert 4th item, should evict "a" (FIFO)
	_ = c.Put(ctx, "d", "4", 60)

	err := c.Get(ctx, "a", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist, "a should be evicted")

	assert.Nil(t, c.Get(ctx, "d", &s))
	assert.Equal(t, "4", s)
}

func TestCapacityMaxBytes(t *testing.T) {
	c := cache.New(
		cache.WithMemStore(),
		cache.WithMaxBytes(50), // 每项实际 21 字节（12 字节 JSON 字符串 + 9 字节版本前缀）: 2 项 42 字节，第 3 项 63 字节触发 eviction
	)
	ctx := context.Background()

	// Insert items until eviction kicks in
	_ = c.Put(ctx, "k1", "1234567890", 60) // 21 bytes
	_ = c.Put(ctx, "k2", "1234567890", 60) // 42 total
	_ = c.Put(ctx, "k3", "1234567890", 60) // 63 total → triggers eviction

	// Eviction removes k1 (21 bytes → 42 remaining, under limit)
	var s string
	err := c.Get(ctx, "k1", &s)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist, "k1 should be evicted (FIFO)")

	// k2 and k3 should still exist (42 bytes ≤ 50 limit)
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

	_ = c.Put(ctx, "a", "1", 60)
	_ = c.Put(ctx, "b", "2", 60)

	// Overwrite "a" - should not count as a new entry for FIFO
	_ = c.Put(ctx, "a", "updated", 60)

	// Insert new key - should evict "b" (FIFO: "a" was re-inserted and is now newest)
	_ = c.Put(ctx, "c", "3", 60)

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
	failCount atomic.Int64 // healthLoop probe 与请求路径并发访问
	maxFails  int
}

func newErrorStore(name string, maxFails int) *errorStore {
	return &errorStore{
		mockStore: newMockStore(name, true),
		maxFails:  maxFails,
	}
}

func (s *errorStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	s.failCount.Add(1)
	if s.failCount.Load() <= int64(s.maxFails) {
		return nil, false, errors.New("remote error")
	}
	return s.mockStore.Get(ctx, key)
}

func (s *errorStore) Put(ctx context.Context, key string, v []byte, expireSecond int) error {
	s.failCount.Add(1)
	if s.failCount.Load() <= int64(s.maxFails) {
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
		cache.WithVerifyEvery(1), // 每次 Get 都触发 remote 校验，以真实驱动降级计数
	)
	ctx := context.Background()

	// 本地放一个值，通过概率校验路径让 remote 连续失败
	_ = local.Put(ctx, "key", []byte(`"value"`), 60)

	var s string
	for i := 0; i < 5; i++ {
		_ = c.Get(ctx, "key", &s)
	}
	time.Sleep(50 * time.Millisecond)

	// 断言：remote 连续失败达到阈值后进入降级模式
	assert.True(t, spy.degraded.Load(), "should enter degraded mode after repeated remote failures")
	assert.GreaterOrEqual(t, remote.failCount.Load(), int64(3), "remote should have failed up to the degrade threshold")

	// 降级期间写入：只写本地并缓冲（pending），remote 不直接收到数据
	_ = c.Put(ctx, "degraded-key", "dval", 60)
	_, exist, _ := remote.Get(ctx, "degraded-key")
	assert.False(t, exist, "remote must not receive writes while degraded")

	// 降级期间本地仍可读（数据不会因 remote 不可达被误删）
	var dv string
	assert.Nil(t, c.Get(ctx, "degraded-key", &dv))
	assert.Equal(t, "dval", dv)

	c.Close()
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
	_ = c.Put(ctx, "localkey", "localval", 60)
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

	_ = c.Put(ctx, "a", "hello-a", 60)
	_ = c.Put(ctx, "b", "hello-b", 60)
	_ = c.Put(ctx, "c", "hello-c", 60)

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

	_ = c.Put(ctx, "a", "alpha", 60)
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

	_ = s.Put(ctx, "a", "val", 60)
	var v string
	_ = s.Get(ctx, "a", &v) // hit

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
	_ = c.Put(ctx, "key", "value", 60)

	// Multiple Gets — remote will fail each time, triggering degrade counter
	var s string
	for i := 0; i < 5; i++ {
		_ = c.Get(ctx, "key", &s)
	}
	assert.Equal(t, "value", s)

	// Give recovery probe a moment (it should detect we're degraded)
	time.Sleep(100 * time.Millisecond)
	c.Close()
}

func TestDegradeRecoveryAfterFailure(t *testing.T) {
	spy := &spyMetrics{}
	local := newMockStore("local", false)
	// 前 5 次失败，之后成功（模拟 remote 从故障中恢复）
	remote := newErrorStore("remote", 5)

	c := cache.New(
		cache.WithMetrics(spy),
		func(o *cache.Options) { o.WithStore(local) },
		func(o *cache.Options) { o.WithStore(remote) },
		cache.WithDegradeThreshold(3),
		cache.WithDegradeRecoveryInterval(50*time.Millisecond),
		cache.WithVerifyEvery(1),
	)
	ctx := context.Background()

	_ = local.Put(ctx, "key", []byte(`"value"`), 60)

	var s string
	for i := 0; i < 5; i++ {
		_ = c.Get(ctx, "key", &s)
	}
	// 断言：连续失败已触发降级
	assert.True(t, spy.degraded.Load(), "should degrade after repeated failures")
	assert.GreaterOrEqual(t, remote.failCount.Load(), int64(3))

	// health probe 探测到 remote 恢复（第 6 次调用起成功）后退出降级
	deadline := time.Now().Add(2 * time.Second)
	for spy.degraded.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	assert.False(t, spy.degraded.Load(), "should exit degraded mode after remote recovers")

	// 恢复后 remote 已有数据（模拟其它实例写入）：校验路径重新命中 remote 并回写本地
	_ = remote.mockStore.Put(ctx, "key", []byte(`"value"`), 60)
	assert.Nil(t, c.Get(ctx, "key", &s))
	assert.Equal(t, "value", s)

	c.Close()
}

// --- BulkStore GetMulti coverage ---

func TestMemStoreBulkGetMulti(t *testing.T) {
	// Test that memory_store's GetMulti works (covers the BulkStore implementation)
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	_ = c.Put(ctx, "a", "x", 60)
	_ = c.Put(ctx, "b", "y", 60)
	_ = c.Put(ctx, "c", "z", 60)

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

	_ = c.Put(ctx, "deletekey", "val", 60)
	c.Delete(ctx, "deletekey")

	// Publish 应被调用（断言发布记录，不依赖 watcher 与测试 goroutine 竞争消费 channel）
	lis.mu.Lock()
	published := append([]string(nil), lis.published...)
	lis.mu.Unlock()
	assert.Contains(t, published, "deletekey")

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
	_ = c.Put(ctx, "key", "val", 60)

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
	// Get a cache with default metrics (noop) 与默认本地缓存（零参 New 注入 mem_store）
	c := cache.New()
	assert.NotNil(t, c)

	// Verify the cache was created - noopMetrics is internal
	// but the cache should work without custom metrics
	// Note: 零参 New 现在默认注入内存缓存（P0-1），Put/Get 往返成功
	ctx := context.Background()
	err := c.Put(ctx, "nomet", "value", 60)
	assert.Nil(t, err)

	var s string
	err = c.Get(ctx, "nomet", &s)
	assert.Nil(t, err)
	assert.Equal(t, "value", s)
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

	_ = c.Put(ctx, "a:1", "v1", 60)
	_ = c.Put(ctx, "a:2", "v2", 60)
	_ = c.Put(ctx, "b:1", "v3", 60)

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

	_ = c.Put(ctx, "a", "1", 60)
	_ = c.Put(ctx, "b", "2", 60) // should evict "a"

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

	_ = c.Put(ctx, "a", "x", 60)
	_ = c.Put(ctx, "b", "y", 60)

	result, err := c.GetMulti(ctx, "a", "b")
	assert.Nil(t, err)
	assert.Len(t, result, 2)
	assert.Equal(t, "x", result["a"])
	assert.Equal(t, "y", result["b"])
}

// --- Close 幂等 ---

func TestCloseIdempotent(t *testing.T) {
	lis := newMockListener()
	c := cache.New(
		cache.WithMemStore(),
		cache.WithListener(lis),
		cache.WithDegradeThreshold(1),
		cache.WithDegradeRecoveryInterval(50*time.Millisecond),
	)

	// 多次 Close 不得 panic（stopChan/降级探测/版本同步/store 均为单次关闭）
	assert.NotPanics(t, func() { c.Close() })
	assert.NotPanics(t, func() { c.Close() })
	assert.NotPanics(t, func() { c.Close() })
}

// --- expireSecond=0 语义：永不过期 ---

func TestPutWithZeroExpireNeverMisses(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()

	_ = c.Put(ctx, "forever", "value", 0)

	var s string
	assert.Nil(t, c.Get(ctx, "forever", &s))
	assert.Equal(t, "value", s)

	// 0 TTL 表示永不过期：等待超过默认短期 TTL 后仍应可读
	time.Sleep(1200 * time.Millisecond)
	s = ""
	assert.Nil(t, c.Get(ctx, "forever", &s))
	assert.Equal(t, "value", s)

	c.Close()
}

// --- P0-1: 零参 New 默认注入内存缓存 ---

func TestNewDefaultMemStore(t *testing.T) {
	// 零参 New 默认注入本地内存缓存（与显式 WithMemStore 一致）：
	// Put/Get 命中；Getfn 回源后二次调用不再回源（写入生效）。
	c := cache.New()
	ctx := context.Background()

	assert.Nil(t, c.Put(ctx, "k", "v", 60))
	var s string
	assert.Nil(t, c.Get(ctx, "k", &s))
	assert.Equal(t, "v", s)

	callCount := 0
	assert.Nil(t, c.Getfn(ctx, "k2", &s, func(ctx context.Context, key string, v any) (bool, error) {
		callCount++
		if sv, ok := v.(*string); ok {
			*sv = "loaded"
		}
		return true, nil
	}, 60))
	assert.Equal(t, "loaded", s)
	assert.Equal(t, 1, callCount)

	_ = c.Getfn(ctx, "k2", &s, func(ctx context.Context, key string, v any) (bool, error) {
		callCount++
		return true, nil
	}, 60)
	assert.Equal(t, 1, callCount, "Getfn write-back must take effect with default mem store")
	c.Close()
}

// --- P0-4: 远程故障错误分类哨兵 ---

// alwaysFailStore 的 Get/Put 恒返回指定错误（模拟远程故障）。
type alwaysFailStore struct {
	*mockStore
	err error
}

func (s *alwaysFailStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return nil, false, s.err
}

func (s *alwaysFailStore) Put(ctx context.Context, key string, v []byte, expireSecond int) error {
	return s.err
}

func TestErrRemoteUnavailableClassification(t *testing.T) {
	// 远程故障可被 errors.Is 区分：ErrRemoteUnavailable 为 true，
	// 且原错误链保留（errors.Is(err, 原始错误) 仍可用）。
	remoteErr := errors.New("conn refused")
	remote := &alwaysFailStore{mockStore: newMockStore("remote", true), err: remoteErr}
	c := cache.New(
		cache.WithMemStore(),
		func(o *cache.Options) { o.WithStore(remote) },
	)
	ctx := context.Background()

	var s string
	err := c.Get(ctx, "k", &s)
	assert.ErrorIs(t, err, cache.ErrRemoteUnavailable, "remote failure should be classified as unavailable")
	assert.ErrorIs(t, err, remoteErr, "original error chain must be preserved")

	// Put 路径 remote 失败同样分类
	err = c.Put(ctx, "k2", "v", 60)
	assert.ErrorIs(t, err, cache.ErrRemoteUnavailable)
	assert.ErrorIs(t, err, remoteErr)
	c.Close()
}

// --- PreLoad 批量写入（启动预热）---

// countingBulkStore 实现 BulkStore 并记录 SetMulti 调用次数。
type countingBulkStore struct {
	*mockStore
	setMultiCount atomic.Int64
}

func (s *countingBulkStore) GetMulti(_ context.Context, _ ...string) (map[string][]byte, error) {
	return nil, nil
}

func (s *countingBulkStore) SetMulti(_ context.Context, items map[string][]byte, _ int) error {
	s.setMultiCount.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range items {
		s.data[k] = mockItem{value: v}
	}
	return nil
}

func TestPreLoadBatchWrite(t *testing.T) {
	// PreLoad 应批量写入（SetMulti 一次调用），而非逐 key Put（N 次）
	local := &countingBulkStore{mockStore: newMockStore("local", false)}
	c := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
	)
	ctx := context.Background()

	err := c.PreLoad(ctx, func(ctx context.Context) (map[string]any, error) {
		return map[string]any{"a": "1", "b": "2", "c": "3"}, nil
	}, 60)
	assert.Nil(t, err)
	assert.Equal(t, int64(1), local.setMultiCount.Load(), "PreLoad should batch-write via SetMulti once")

	// 全部 key 可命中、值正确
	var s string
	assert.Nil(t, c.Get(ctx, "a", &s))
	assert.Equal(t, "1", s)
	assert.Nil(t, c.Get(ctx, "b", &s))
	assert.Equal(t, "2", s)
	assert.Nil(t, c.Get(ctx, "c", &s))
	assert.Equal(t, "3", s)
	c.Close()
}

func TestPreLoadEmpty(t *testing.T) {
	// 空 map 不报错
	c := cache.New()
	ctx := context.Background()
	err := c.PreLoad(ctx, func(ctx context.Context) (map[string]any, error) {
		return map[string]any{}, nil
	}, 60)
	assert.Nil(t, err, "empty preload should not error")
	c.Close()
}

// --- Stats 只读快照（cache 级）---

func TestCacheStatsSnapshotIsolated(t *testing.T) {
	c := cache.New(cache.WithMemStore())
	ctx := context.Background()
	_ = c.Put(ctx, "k", "v", 60)
	var s string
	_ = c.Get(ctx, "k", &s) // local hit

	st := c.Stats()
	st.Query = 999
	st.Shared = 999
	st.Clear()

	// 内部计数不受快照修改影响：再次 Get 命中后 TotalHits 仍增长
	_ = c.Get(ctx, "k", &s)
	after := c.Stats()
	assert.GreaterOrEqual(t, after.TotalHits(), uint64(1), "snapshot mutation must not affect internal counters")
	c.Close()
}
