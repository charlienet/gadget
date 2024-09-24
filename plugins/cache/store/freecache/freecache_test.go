package freecache_test

import (
	"context"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/plugins/cache/store/freecache"
	"github.com/stretchr/testify/assert"
)

func TestCache(t *testing.T) {
	ctx := context.TODO()
	key := "redistestkey"
	val := "hello go-cache"

	t.Run("CacheGetMiss", func(t *testing.T) {
		if err := cache.New(freecache.New(1000)).Get(ctx, key, nil); err == nil {
			t.Error("expected to get no value from cache")
		}
	})

	t.Run("CacheGetHit", func(t *testing.T) {
		c := cache.New(freecache.New(1000))

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
		c := cache.New(freecache.New(1000))
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
}

func BenchmarkFreecache(b *testing.B) {
	key := "key"

	c := cache.New(freecache.New(1000))

	b.Run("b", func(b *testing.B) {
		for range b.N {
			c.Get(context.Background(), key, "abc")
		}
	})

}
