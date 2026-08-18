package bigcache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore builds a bigcache store for tests, failing the test on
// construction error and registering Close as cleanup.
func newTestStore(t *testing.T, configs ...ConfigFunc) *bigcache_store {
	t.Helper()
	s, err := NewBigCache(configs...)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s
}

func TestStoreMeta(t *testing.T) {
	s := newTestStore(t)

	assert.Equal(t, "bigcache", s.Name())
	assert.False(t, s.IsRemote())
}

func TestStoreGetHitAndMiss(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Miss: a missing key returns (nil, false, nil) without error.
	v, ok, err := s.Get(ctx, "missing")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, v)

	// Hit.
	require.NoError(t, s.Put(ctx, "key", []byte("value"), 0))
	v, ok, err = s.Get(ctx, "key")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, []byte("value"), v)
}

func TestStoreDeleteIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.Put(ctx, "key", []byte("value"), 0))
	require.NoError(t, s.Delete(ctx, "key"))
	// Deleting a missing key must be a no-op, not an error.
	require.NoError(t, s.Delete(ctx, "key"))
	require.NoError(t, s.Delete(ctx, "missing", "another"))

	_, ok, err := s.Get(ctx, "key")
	require.NoError(t, err)
	assert.False(t, ok, "key must be deleted")
}

func TestStoreExpiresAfterLifeWindow(t *testing.T) {
	// bigcache has a global LifeWindow: entries expire even when written with
	// expireSeconds=0. Eviction happens during background cleanups (CleanWindow)
	// — Get alone does not lazily expire — so LifeWindow must be >= 1s (the
	// library's time resolution is seconds) and CleanWindow must be small
	// enough to evict within the test window.
	s := newTestStore(t, WithLifeWindow(time.Second), WithCleanWindow(100*time.Millisecond))
	ctx := context.Background()

	require.NoError(t, s.Put(ctx, "key", []byte("value"), 0))
	<-time.After(2500 * time.Millisecond)

	_, ok, err := s.Get(ctx, "key")
	require.NoError(t, err)
	assert.False(t, ok, "entry must expire after the global LifeWindow")
}

func TestInitializeLinksGlobalTTL(t *testing.T) {
	// When the cache package sets a global TTL (cache.WithTTL), the store's
	// LifeWindow is linked to it through the Initialize hook. A small
	// CleanWindow keeps the test window short.
	s := newTestStore(t, WithCleanWindow(100*time.Millisecond))

	s.Initialize(cache.Options{TTL: 1})
	ctx := context.Background()

	require.NoError(t, s.Put(ctx, "key", []byte("value"), 0))
	<-time.After(2500 * time.Millisecond)

	_, ok, err := s.Get(ctx, "key")
	require.NoError(t, err)
	assert.False(t, ok, "entry must expire after the configured global TTL")
}

// TestCacheCloseInvokesStoreClose verifies that the cache package's Close()
// reaches the bigcache store through its no-return-value Close() probe
// (interface{ Close() }, see cache/cache.go). A Close() error signature would
// fail that type assertion and leak the store's cleanup goroutines.
func TestCacheCloseInvokesStoreClose(t *testing.T) {
	// Note: no t.Cleanup(s.Close) here — cache.Close() below already closes the
	// store (the underlying library's Close is not idempotent), and the store
	// must be closed by this test's cache instance.
	s, err := NewBigCache()
	require.NoError(t, err)
	c := cache.New(func(o *cache.Options) { o.WithStore(s) })

	if s.closed {
		t.Fatal("store must not be closed before cache.Close()")
	}

	c.Close()

	if !s.closed {
		t.Error("cache.Close() must invoke the bigcache store's Close()")
	}
}

// TestNewBigCacheValidatesConfig covers the construction-time validation of
// WithLifeWindow/WithCleanWindow/WithShards values that would otherwise
// silently misbehave in the underlying library: invalid configurations must
// surface as errors, valid boundary values must be accepted.
func TestNewBigCacheValidatesConfig(t *testing.T) {
	t.Run("LifeWindowBelowOneSecondRejected", func(t *testing.T) {
		_, err := NewBigCache(WithLifeWindow(900 * time.Millisecond))
		require.Error(t, err, "LifeWindow below 1s must be rejected")
	})

	t.Run("LifeWindowExactlyOneSecondAccepted", func(t *testing.T) {
		newTestStore(t, WithLifeWindow(time.Second))
	})

	t.Run("CleanWindowNonPositiveRejected", func(t *testing.T) {
		for _, d := range []time.Duration{0, -time.Second} {
			_, err := NewBigCache(WithCleanWindow(d))
			require.Error(t, err, "CleanWindow=%v must be rejected", d)
		}
	})

	t.Run("CleanWindowPositiveAccepted", func(t *testing.T) {
		newTestStore(t, WithCleanWindow(100*time.Millisecond))
	})

	t.Run("ShardsInvalidRejected", func(t *testing.T) {
		for _, n := range []int{0, 3, 100, 4097} {
			_, err := NewBigCache(WithShards(n))
			require.Error(t, err, "Shards=%d must be rejected", n)
		}
	})

	t.Run("ShardsMaxAccepted", func(t *testing.T) {
		newTestStore(t, WithShards(4096))
	})
}

// TestPutRejectsOversizedValue covers the P0 pre-check: values larger than the
// per-shard queue limit must be rejected up front, because the underlying
// library would otherwise evict every entry of that shard before failing.
func TestPutRejectsOversizedValue(t *testing.T) {
	// HardMaxCacheSize=1MB, Shards=2 → per-shard limit = 512KB.
	s := newTestStore(t, WithHardMaxCacheSize(1), WithShards(2))
	ctx := context.Background()

	require.NoError(t, s.Put(ctx, "keep", []byte("small"), 0))

	big := make([]byte, 600*1024) // 600KB > 512KB per-shard limit
	err := s.Put(ctx, "big", big, 0)
	require.Error(t, err, "oversized value must be rejected up front")
	assert.Contains(t, err.Error(), "per-shard limit")

	// The rejected write must not have evicted existing entries.
	v, ok, err := s.Get(ctx, "keep")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, []byte("small"), v)
}

// TestPutSkipsPrecheckWhenHardLimitUnlimited verifies that the oversized-value
// pre-check is disabled when HardMaxCacheSize is 0 (unlimited).
func TestPutSkipsPrecheckWhenHardLimitUnlimited(t *testing.T) {
	s := newTestStore(t, WithHardMaxCacheSize(0))
	ctx := context.Background()

	big := make([]byte, 3*1024*1024) // 3MB, far above any default shard limit
	err := s.Put(ctx, "big", big, 0)
	require.NotContains(t, fmt.Sprint(err), "per-shard limit",
		"pre-check must be skipped when HardMaxCacheSize=0")
}

// TestConcurrentPutGetDelete is a smoke test for concurrent Put/Get/Delete on
// the same key: it must not panic, hits must carry non-empty values (the last
// writer wins, no exact value assertion), and a final Delete must yield a
// stable miss. Must pass with -race.
func TestConcurrentPutGetDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const (
		goroutines = 20
		rounds     = 50
	)
	key := "contended-key"

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				switch r % 3 {
				case 0, 1:
					val := []byte(fmt.Sprintf("v-%d-%d", id, r))
					if err := s.Put(ctx, key, val, 0); err != nil {
						t.Errorf("goroutine %d Put: %v", id, err)
						return
					}
				case 2:
					if err := s.Delete(ctx, key); err != nil {
						t.Errorf("goroutine %d Delete: %v", id, err)
						return
					}
				}

				v, ok, err := s.Get(ctx, key)
				if err != nil {
					t.Errorf("goroutine %d Get: %v", id, err)
					return
				}
				if ok && len(v) == 0 {
					t.Errorf("goroutine %d: hit with empty value", id)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	// Concurrent phase over: Delete must yield a stable miss.
	require.NoError(t, s.Delete(ctx, key))
	_, ok, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.False(t, ok, "expected miss after final Delete")
}
