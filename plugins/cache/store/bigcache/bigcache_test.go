package bigcache_test

import (
	"context"
	"testing"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/plugins/cache/store/bigcache"
	"github.com/stretchr/testify/assert"
)

func TestCache(t *testing.T) {
	ctx := context.TODO()
	key := "redistestkey"
	val := "hello go-cache"

	t.Run("CacheGetMiss", func(t *testing.T) {
		if err := cache.New(bigcache.New()).Get(ctx, key, nil); err == nil {
			t.Error("expected to get no value from cache")
		}
	})

	t.Run("CacheGetHit", func(t *testing.T) {
		c := cache.New(bigcache.New())

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
}
