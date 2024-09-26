package cache_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/go-misc/json"
	"github.com/stretchr/testify/assert"
)

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
		json.Unmarshal([]byte(str), &v)

		return true, nil
	}

	c.Getfn(ctx, "dummy-key", &v, loadfn, 2)

	for range 10 {
		c.Getfn(ctx, "dummy-key", &v, loadfn, 2)
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

	u := User{}

	g := 10
	wg.Add(g)
	for range g {
		go func() {
			defer wg.Done()

			assert.Nil(t, c.Getfn(ctx, key, &u, fn, 30))
			assert.Nil(t, c.Getfn(ctx, key, &u, fn, 30))
			assert.Equal(t, j, json.Struct2Json(u))
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
