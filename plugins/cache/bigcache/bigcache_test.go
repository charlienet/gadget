package bigcache_test

import (
	"context"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/plugins/cache/bigcache"
	"github.com/stretchr/testify/assert"
)

func TestCache(t *testing.T) {
	ctx := context.TODO()
	key := "testkey"
	val := "hello go-cache"

	t.Run("CacheGetMiss", func(t *testing.T) {
		o, err := bigcache.New()
		if err != nil {
			t.Fatal(err)
		}
		c := cache.New(o)
		t.Cleanup(c.Close)

		if err := c.Get(ctx, key, nil); err == nil {
			t.Error("expected to get no value from cache")
		}
	})

	t.Run("CacheGetHit", func(t *testing.T) {
		// NOTE: bigcache does not support per-key TTL. Passing expireSecond=0
		// ("never expire" per the cache.Store contract) is NOT honored per key:
		// the global LifeWindow (default 1 minute) applies. This test passes
		// only because the value is read immediately after writing, so it does
		// NOT prove "never expire" semantics — see CacheExpiresAfterShortLifeWindow.
		o, err := bigcache.New()
		if err != nil {
			t.Fatal(err)
		}
		c := cache.New(o)
		t.Cleanup(c.Close)

		if err := c.Put(ctx, key, val, 0); err != nil {
			t.Error(err)
		}

		var s string
		if err := c.Get(ctx, key, &s); err != nil {
			t.Errorf("Expected a value, got err: %s", err)
		} else if string(s) != val {
			t.Errorf("Expected '%v', got '%v'", val, s)
		}

		assert.Equal(t, val, s)
	})

	t.Run("CacheExpiresAfterShortLifeWindow", func(t *testing.T) {
		// bigcache evicts every entry once the global LifeWindow elapses,
		// regardless of the per-key expireSecond passed to Put (here 0).
		// Eviction only happens during background cleanups, so a small
		// CleanWindow keeps the test window short (LifeWindow must be >= 1s,
		// the library's time resolution).
		o, err := bigcache.New(
			bigcache.WithLifeWindow(time.Second),
			bigcache.WithCleanWindow(100*time.Millisecond),
		)
		if err != nil {
			t.Fatal(err)
		}
		c := cache.New(o)
		t.Cleanup(c.Close)

		if err := c.Put(ctx, key, val, 0); err != nil {
			t.Error(err)
		}

		<-time.After(2500 * time.Millisecond)

		var s string
		if err := c.Get(ctx, key, &s); err == nil {
			t.Error("expected entry to expire after the global LifeWindow")
		}
	})
}

func BenchmarkBigcache(b *testing.B) {
	key := "key"
	val := "hello go-cache"

	o, err := bigcache.New()
	if err != nil {
		b.Fatal(err)
	}
	c := cache.New(o)
	if err := c.Put(context.Background(), key, val, 0); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(c.Close)

	b.Run("GetHit", func(b *testing.B) {
		for range b.N {
			_ = c.Get(context.Background(), key, "abc")
		}
	})

	b.Run("GetMiss", func(b *testing.B) {
		for range b.N {
			_ = c.Get(context.Background(), "missing", "abc")
		}
	})
}
