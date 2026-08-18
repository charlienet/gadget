package freecache_test

import (
	"context"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/plugins/cache/freecache"
	"github.com/stretchr/testify/assert"
)

func TestCache(t *testing.T) {
	ctx := context.TODO()
	key := "testkey"
	val := "hello go-cache"

	t.Run("CacheGetMiss", func(t *testing.T) {
		o, err := freecache.New(1000)
		if err != nil {
			t.Fatal(err)
		}

		if err := cache.New(o).Get(ctx, key, nil); err == nil {
			t.Error("expected to get no value from cache")
		}
	})

	t.Run("CacheGetHit", func(t *testing.T) {
		o, err := freecache.New(1000)
		if err != nil {
			t.Fatal(err)
		}
		c := cache.New(o)

		if err := c.Put(ctx, key, val, 30); err != nil {
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

	t.Run("CacheGetExpired", func(t *testing.T) {
		o, err := freecache.New(1000)
		if err != nil {
			t.Fatal(err)
		}
		c := cache.New(o)
		d := 2

		if err := c.Put(ctx, key, val, d); err != nil {
			t.Error(err)
		}

		var s string
		<-time.After(5 * time.Second)
		if err := c.Get(ctx, key, &s); err == nil {
			t.Error("expected to get no value from cache")
		}
	})

	t.Run("CacheTTLZeroNeverExpires", func(t *testing.T) {
		o, err := freecache.New(1000)
		if err != nil {
			t.Fatal(err)
		}
		c := cache.New(o)

		// expireSeconds=0 means "never expire" per the cache.Store contract.
		if err := c.Put(ctx, key, val, 0); err != nil {
			t.Error(err)
		}

		<-time.After(2 * time.Second)

		var s string
		if err := c.Get(ctx, key, &s); err != nil {
			t.Errorf("expected value to persist with expireSeconds=0, got err: %s", err)
		}
		assert.Equal(t, val, s)
	})
}

func BenchmarkFreecache(b *testing.B) {
	key := "key"
	val := "hello go-cache"

	o, err := freecache.New(1000)
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
