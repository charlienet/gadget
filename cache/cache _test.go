package cache_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
)

type cacheItem struct {
	Name string
}

func TestNewCache(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {

		c := cache.New(cache.WithRedis(rdb), cache.WithBigcache())

		ctx := context.Background()
		item := cacheItem{}
		c.Set(ctx, "abc", cacheItem{Name: "test"}, 5)
		c.Get(ctx, "abc", &item)

		b, _ := json.MarshalIndent(item, "  ", "")
		t.Log(string(b))
	})
}

func TestLoadFromFunc(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		c := cache.New(cache.WithRedis(rdb), cache.WithFreecache())

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

		for i := 0; i < 10; i++ {
			c.Getfn(ctx, "dummy-key", &v, loadfn, 2)
		}

		b, _ := json.Marshal(v)

		t.Log(v.Name)
		t.Log(string(b))
	})
}

func TestNoCache(t *testing.T) {
	c := cache.New()
	c.Disable()

	ctx := context.Background()
	var item cacheItem

	t.Log(c.Getfn(ctx, "ttt", &item, func(ctx context.Context, key string, v any) (bool, error) {
		typ := reflect.TypeOf(v)
		_ = typ

		if value, ok := v.(*cacheItem); ok {
			t.Log("is here")
			value.Name = "cccccccc"
		}
		return true, nil
	}, 20))

	b, _ := json.Marshal(item)
	t.Log(string(b))
}

func TestSourceError(t *testing.T) {
	c := cache.New(cache.WithBigcache())
	t.Log(c.Getfn(context.Background(), "abc", map[string]any{}, func(ctx context.Context, key string, v any) (bool, error) {
		return false, errors.New("data source load error")
	}, 20))
}
