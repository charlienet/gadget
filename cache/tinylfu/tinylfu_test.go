package tinylfu

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type StoreItem struct {
	Name string
}

func TestGetSet(t *testing.T) {
	c := NewTinyLFU(10, time.Minute)
	t.Log(c.Get(context.TODO(), "abc"))

	v := []byte("abc")
	c.Set(context.Background(), "abc", v, 20)

	data, exist, err := c.Get(context.Background(), "abc")
	t.Log(string(data), exist, err)
}

func TestCache(t *testing.T) {
	cache := NewTinyLFU(1e3, 10e3)
	keys := []string{"one", "two", "three"}

	ctx := context.Background()
	for _, key := range keys {
		cache.Set(ctx, key, []byte(key), 0)

		got, ok, _ := cache.Get(ctx, key)
		require.True(t, ok)
		require.Equal(t, []byte(key), got)
	}

	for _, key := range keys {
		got, ok, _ := cache.Get(ctx, key)
		require.True(t, ok)
		require.Equal(t, []byte(key), got)

		cache.Set(ctx, key, []byte(key+key), 0)
	}

	for _, key := range keys {
		got, ok, _ := cache.Get(ctx, key)
		require.True(t, ok)
		require.Equal(t, []byte(key+key), got)
	}

	for _, key := range keys {
		cache.Delete(ctx, key)
	}

	for _, key := range keys {
		_, ok, _ := cache.Get(ctx, key)
		require.False(t, ok)
	}
}
