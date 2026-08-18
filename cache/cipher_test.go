package cache_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/stretchr/testify/assert"
)

// xorCipher 是测试用简单加解密器（XOR 0x5A，自反），非安全用途。
// 用于验证透明加解密路径：缓存中存储密文，调用方明文进出。
type xorCipher struct{}

func (xorCipher) Encrypt(plaintext []byte) ([]byte, error) {
	out := make([]byte, len(plaintext))
	for i, b := range plaintext {
		out[i] = b ^ 0x5A
	}
	return out, nil
}

func (xorCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	return (xorCipher{}).Encrypt(ciphertext)
}

// stripVersion 剥离缓存存储中的版本前缀（1 magic byte + 8 字节时间戳，
// 与 cache 包内部 versionPrefixLen 一致），返回负载。
func stripVersion(data []byte) []byte {
	if len(data) > 9 && data[0] == 0xFB {
		return data[9:]
	}
	return data
}

func TestPutGetWithCipher(t *testing.T) {
	remote := newMockStore("remote", true)
	c := cache.New(
		cache.WithMemStore(),
		func(o *cache.Options) { o.WithStore(remote) },
		cache.WithCipher(xorCipher{}),
	)
	ctx := context.Background()

	assert.Nil(t, c.Put(ctx, "k", "secret", 60))

	// 往返明文正确
	var s string
	assert.Nil(t, c.Get(ctx, "k", &s))
	assert.Equal(t, "secret", s)

	// L2（remote）存储的是密文而非明文
	data, exist, _ := remote.Get(ctx, "k")
	assert.True(t, exist)
	assert.NotContains(t, string(data), `"secret"`, "remote must store ciphertext, not plaintext")
	// 解密后恢复明文（版本前缀原样明文）
	plain, err := (xorCipher{}).Decrypt(stripVersion(data))
	assert.Nil(t, err)
	assert.Equal(t, []byte(`"secret"`), plain)
}

func TestGetfnWithCipher(t *testing.T) {
	remote := newMockStore("remote", true)
	c := cache.New(
		cache.WithMemStore(),
		func(o *cache.Options) { o.WithStore(remote) },
		cache.WithCipher(xorCipher{}),
	)
	ctx := context.Background()

	var s string
	assert.Nil(t, c.Getfn(ctx, "k", &s, func(ctx context.Context, key string, v any) (bool, error) {
		if sv, ok := v.(*string); ok {
			*sv = "loaded"
		}
		return true, nil
	}, 60))
	assert.Equal(t, "loaded", s)

	// L2 存密文
	data, exist, _ := remote.Get(ctx, "k")
	assert.True(t, exist)
	assert.NotContains(t, string(data), `"loaded"`)
	plain, err := (xorCipher{}).Decrypt(stripVersion(data))
	assert.Nil(t, err)
	assert.Equal(t, []byte(`"loaded"`), plain)
}

func TestGetfnSourceNotFoundWithCipher(t *testing.T) {
	remote := newMockStore("remote", true)
	c := cache.New(
		cache.WithMemStore(),
		func(o *cache.Options) { o.WithStore(remote) },
		cache.WithCipher(xorCipher{}),
	)
	ctx := context.Background()

	var s string
	err := c.Getfn(ctx, "missing", &s, func(ctx context.Context, key string, v any) (bool, error) {
		return false, nil
	}, 60)
	assert.ErrorIs(t, err, cache.ErrEntityNotExist)

	// 占位符仍以明文 "*" 存储（不加密）
	data, exist, _ := remote.Get(ctx, "missing")
	assert.True(t, exist)
	assert.Equal(t, []byte("*"), stripVersion(data), "placeholder must be stored as plaintext")
}

func TestConcurrentGetfnWithCipher(t *testing.T) {
	c := cache.New(
		cache.WithMemStore(),
		cache.WithCipher(xorCipher{}),
	)
	ctx := context.Background()

	var mu sync.Mutex
	loadCount := 0
	loadFn := func(ctx context.Context, key string, v any) (bool, error) {
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		loadCount++
		mu.Unlock()
		if s, ok := v.(*string); ok {
			*s = "concurrent"
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
			assert.Nil(t, c.Getfn(ctx, "ckey", &result, loadFn, 60))
			assert.Equal(t, "concurrent", result)
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, 1, loadCount, "singleflight dedup must hold under cipher")
}

func TestSetMultiGetMultiWithCipher(t *testing.T) {
	ctx := context.Background()

	// bulk 路径（mem_store 实现 BulkStore）
	bulkCache := cache.New(
		cache.WithMemStore(),
		cache.WithCipher(xorCipher{}),
	)
	assert.Nil(t, bulkCache.SetMulti(ctx, map[string]any{"a": "va", "b": "vb"}, 60))
	res, err := bulkCache.GetMulti(ctx, "a", "b")
	assert.Nil(t, err)
	assert.Equal(t, "va", res["a"])
	assert.Equal(t, "vb", res["b"])

	// fallback 路径（mockStore 非 BulkStore）
	local := newMockStore("local", false)
	fallbackCache := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
		cache.WithCipher(xorCipher{}),
	)
	assert.Nil(t, fallbackCache.SetMulti(ctx, map[string]any{"x": "vx"}, 60))
	res2, err := fallbackCache.GetMulti(ctx, "x")
	assert.Nil(t, err)
	assert.Equal(t, "vx", res2["x"])
	// fallback 写入的也是密文
	data, exist, _ := local.Get(ctx, "x")
	assert.True(t, exist)
	assert.NotContains(t, string(data), `"vx"`)
}

// failCipher 的 Encrypt 恒失败，用于验证 seal 错误传播。
type failCipher struct{}

func (failCipher) Encrypt([]byte) ([]byte, error) { return nil, fmt.Errorf("encrypt boom") }
func (failCipher) Decrypt([]byte) ([]byte, error) { return nil, fmt.Errorf("decrypt boom") }

func TestCipherErrorPropagates(t *testing.T) {
	c := cache.New(
		cache.WithMemStore(),
		cache.WithCipher(failCipher{}),
	)
	ctx := context.Background()

	// Put：seal（Encrypt）失败 → 返回错误
	err := c.Put(ctx, "k", "v", 60)
	assert.ErrorContains(t, err, "encrypt boom")

	// Getfn：fn 回填时 putCache 的 seal 失败被忽略（不影响回源结果）
	var s string
	assert.Nil(t, c.Getfn(ctx, "k2", &s, func(ctx context.Context, key string, v any) (bool, error) {
		if sv, ok := v.(*string); ok {
			*sv = "val"
		}
		return true, nil
	}, 60))
	assert.Equal(t, "val", s)

	// 直接写入 store 密文后 Get：unseal（Decrypt）失败 → 返回错误
	local := newMockStore("local", false)
	_ = local.Put(ctx, "k3", []byte{0xFB, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 60) // 版本前缀 + 任意密文
	c2 := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
		cache.WithCipher(failCipher{}),
	)
	var v2 string
	err = c2.Get(ctx, "k3", &v2)
	assert.ErrorContains(t, err, "decrypt boom")
}

func TestNilCipherIgnored(t *testing.T) {
	// WithCipher(nil) 忽略 → 行为与不注入一致
	c := cache.New(
		cache.WithMemStore(),
		cache.WithCipher(nil),
	)
	ctx := context.Background()
	assert.Nil(t, c.Put(ctx, "k", "v", 60))
	var s string
	assert.Nil(t, c.Get(ctx, "k", &s))
	assert.Equal(t, "v", s)
}

// bulkReadStore 实现 BulkStore（GetMulti 读 mockStore.data），用于覆盖
// GetMulti 分派路径的 unseal 错误分支。
type bulkReadStore struct {
	*mockStore
}

func (s *bulkReadStore) GetMulti(_ context.Context, keys ...string) (map[string][]byte, error) {
	res := make(map[string][]byte, len(keys))
	for _, k := range keys {
		if item, ok := s.data[k]; ok {
			res[k] = item.value
		}
	}
	return res, nil
}

func (s *bulkReadStore) SetMulti(_ context.Context, _ map[string][]byte, _ int) error {
	return nil
}

func TestGetMultiBulkUnsealError(t *testing.T) {
	// GetMulti 分派路径：store 中坏密文 → unseal 失败传播
	local := &bulkReadStore{mockStore: newMockStore("local", false)}
	_ = local.Put(context.Background(), "k", []byte{0xFB, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 60) // 版本前缀 + 坏密文
	c := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
		cache.WithCipher(failCipher{}),
	)
	_, err := c.GetMulti(context.Background(), "k")
	assert.ErrorContains(t, err, "decrypt boom")
}

func TestSetMultiSealError(t *testing.T) {
	// bulk 分支（mem_store 实现 BulkStore）：seal 失败传播
	c := cache.New(
		cache.WithMemStore(),
		cache.WithCipher(failCipher{}),
	)
	err := c.SetMulti(context.Background(), map[string]any{"a": "1"}, 60)
	assert.ErrorContains(t, err, "encrypt boom")

	// fallback 分支（mockStore 非 BulkStore）：seal 失败传播
	local := newMockStore("local", false)
	c2 := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
		cache.WithCipher(failCipher{}),
	)
	err = c2.SetMulti(context.Background(), map[string]any{"b": "2"}, 60)
	assert.ErrorContains(t, err, "encrypt boom")
}

func TestGetfnUnsealError(t *testing.T) {
	// Getfn 缓存命中 → unseal 失败传播
	local := newMockStore("local", false)
	_ = local.Put(context.Background(), "k", []byte{0xFB, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 60) // 版本前缀 + 坏密文
	c := cache.New(
		func(o *cache.Options) { o.WithStore(local) },
		cache.WithCipher(failCipher{}),
	)
	var s string
	err := c.Getfn(context.Background(), "k", &s, func(ctx context.Context, key string, v any) (bool, error) {
		return true, nil
	}, 60)
	assert.ErrorContains(t, err, "decrypt boom")
}

func TestConcurrentGetShared(t *testing.T) {
	// 并发 Get 同 key → getFromCache 的 singleflight 共享计数。
	// 用 verifyEvery(1) + 慢 remote 扩大 getFromCache 闭包窗口，
	// 使并发 Get 稳定进入 singleflight 共享。
	remote := &slowRemoteStore{mockStore: newMockStore("remote", true)}
	c := cache.New(
		cache.WithMemStore(),
		func(o *cache.Options) { o.WithStore(remote) },
		cache.WithVerifyEvery(1),
	)
	ctx := context.Background()
	_ = c.Put(ctx, "k", "v", 60) // local + remote 均有数据

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var s string
			assert.Nil(t, c.Get(ctx, "k", &s))
			assert.Equal(t, "v", s)
		}()
	}
	close(start)
	wg.Wait()

	assert.GreaterOrEqual(t, c.Stats().Shared, uint64(1), "concurrent Get should share singleflight result")
	c.Close()
}
