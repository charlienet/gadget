package cache

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCloserStore 是实现了 io.Closer 的最小 Store：记录 Close 调用次数，
// 按配置返回关闭错误（closeErr），用于锁定 Cache.Close 的级联与聚合契约。
type fakeCloserStore struct {
	name     string
	remote   bool
	closeErr error

	mu        sync.Mutex
	closeCall int
}

func (s *fakeCloserStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}
func (s *fakeCloserStore) Put(context.Context, string, []byte, int) error { return nil }
func (s *fakeCloserStore) Delete(context.Context, ...string) error        { return nil }
func (s *fakeCloserStore) Name() string                                   { return s.name }
func (s *fakeCloserStore) IsRemote() bool                                 { return s.remote }

func (s *fakeCloserStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCall++
	return s.closeErr
}

func (s *fakeCloserStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCall
}

// newBareCache 构造仅够 Close 链路使用的最小 cache 实例（不启动后台协程）。
func newBareCache(local, remote Store) *cache {
	return &cache{
		localStore:  local,
		remoteStore: remote,
		logger:      slog.Default(),
		stopChan:    make(chan struct{}),
	}
}

var (
	errLocalClose  = errors.New("local store close boom")
	errRemoteClose = errors.New("remote store close boom")
)

// 契约①：实现了 io.Closer 的 local/remote store 必须被 Cache.Close 真实级联调用。
func TestCloseCascadesToCloserStores(t *testing.T) {
	local := &fakeCloserStore{name: "fake-local"}
	remote := &fakeCloserStore{name: "fake-remote", remote: true}
	c := newBareCache(local, remote)

	require.NoError(t, c.Close())
	assert.Equal(t, 1, local.calls(), "localStore 应被级联 Close 恰好一次")
	assert.Equal(t, 1, remote.calls(), "remoteStore 应被级联 Close 恰好一次")
}

// 契约①（负向）：未实现 io.Closer 的 store 被自然跳过，不 panic。
func TestCloseSkipsNonCloserStores(t *testing.T) {
	c := newBareCache(&mockLocalStore{data: make(map[string][]byte)}, &mockLocalStore{data: make(map[string][]byte)})
	assert.NotPanics(t, func() { assert.NoError(t, c.Close()) })
}

// 契约②：单 store 返回 error 时，聚合错误可 errors.Is 到 sentinel 且文案含 store 名。
func TestCloseAggregatesSingleStoreError(t *testing.T) {
	remote := &fakeCloserStore{name: "fake-remote", remote: true, closeErr: errRemoteClose}
	c := newBareCache(nil, remote)

	err := c.Close()
	require.Error(t, err)
	assert.ErrorIs(t, err, errRemoteClose)
	assert.Contains(t, err.Error(), "cache: fake-remote store close:")
}

// 契约②：双 store 同时失败时，两个 sentinel 均可 errors.Is 到，且各自文案含 store 名。
func TestCloseAggregatesBothStoreErrors(t *testing.T) {
	local := &fakeCloserStore{name: "fake-local", closeErr: errLocalClose}
	remote := &fakeCloserStore{name: "fake-remote", remote: true, closeErr: errRemoteClose}
	c := newBareCache(local, remote)

	err := c.Close()
	require.Error(t, err)
	assert.ErrorIs(t, err, errLocalClose)
	assert.ErrorIs(t, err, errRemoteClose)
	assert.Contains(t, err.Error(), "cache: fake-local store close:")
	assert.Contains(t, err.Error(), "cache: fake-remote store close:")
}

// 契约②（全 nil）：无任何 store（或 store 均不报错）时 Close 返回 nil。
func TestCloseAllNilStoresReturnsNil(t *testing.T) {
	c := newBareCache(nil, nil)
	assert.NoError(t, c.Close())
}

// 契约③：Close 重复调用返回同一错误（首次结果持久化），store 不被二次关闭（无重试语义）。
func TestCloseRepeatedReturnsSameError(t *testing.T) {
	remote := &fakeCloserStore{name: "fake-remote", remote: true, closeErr: errRemoteClose}
	c := newBareCache(nil, remote)

	err1 := c.Close()
	err2 := c.Close()
	err3 := c.Close()

	require.Error(t, err1)
	assert.Equal(t, err1, err2, "第二次调用应返回与首次完全相同的结果")
	assert.Equal(t, err1, err3)
	assert.ErrorIs(t, err2, errRemoteClose)
	assert.Equal(t, 1, remote.calls(), "关闭主体单次执行，store.Close 不得被重复调用")
}

// 契约③（并发）：并发 Close 全部返回同一结果，store 仅被级联关闭一次（-race 验证）。
func TestCloseConcurrentSameResult(t *testing.T) {
	local := &fakeCloserStore{name: "fake-local", closeErr: errLocalClose}
	remote := &fakeCloserStore{name: "fake-remote", remote: true}
	c := newBareCache(local, remote)

	const n = 8
	results := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = c.Close()
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		require.Error(t, err, "results[%d]", i)
		assert.ErrorIs(t, err, errLocalClose, "results[%d]", i)
	}
	assert.Equal(t, 1, local.calls())
	assert.Equal(t, 1, remote.calls())
	// errors.Join 聚合结果的文案结构稳定：单条、含 store 名。
	all := errors.Join(results...)
	assert.NotNil(t, all)
	assert.True(t, strings.Contains(results[0].Error(), "cache: fake-local store close:"))
}
