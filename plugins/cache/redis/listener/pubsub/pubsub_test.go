package pubsub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
	"github.com/stretchr/testify/assert"
)

func randomHex(n int) string {
	bytes := make([]byte, n/2+1) // Generate enough bytes
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	hexStr := hex.EncodeToString(bytes)
	return hexStr[:n] // Return only n characters
}

// TestWithPublishTimeout 验证 F4：Publish 超时可配置（纯逻辑，无需 Redis）。
func TestWithPublishTimeout(t *testing.T) {
	r := &pubSubListener{publishTimeout: defaultPublishTimeout}
	WithPublishTimeout(5 * time.Second)(r)
	assert.Equal(t, 5*time.Second, r.publishTimeout)
}

// waitReady 等待 listener 订阅首次建立（带超时）。
//
// NewListener 异步启动订阅 goroutine，返回时订阅可能尚未建立；PubSub 广播
// 为 at-most-once，订阅建立前发布的消息会丢失。因此发布前必须先等就绪，
// 不能用固定 sleep（时序不稳定）。通过类型断言获取 Ready（不修改
// cache.Listener 接口签名）。
func waitReady(t *testing.T, lis cache.Listener) {
	t.Helper()

	ready, ok := lis.(interface{ Ready() <-chan struct{} })
	if !ok {
		t.Fatal("listener does not expose Ready")
	}

	select {
	case <-ready.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("listener subscription not ready within 3s")
	}
}

// TestSS 验证发布到订阅 channel 的消息全部送达（广播），
// 其他 channel 的消息不串扰。
func TestSS(t *testing.T) {
	test.RunOnRedisStack(t, func(rdb redis.Client) {
		c := "abc"
		c2 := "abc:dddd"
		r := NewListener(rdb, c)
		defer func() { _ = r.Close(context.Background()) }()

		// 发布 10 条随机 key 作为预期收到的消息集合
		published := make([]string, 0, 10)
		for range 10 {
			published = append(published, randomHex(12))
		}

		var (
			mu   sync.Mutex
			keys []string
		)
		go func() {
			ch := r.Subscribe()
			for key := range ch {
				mu.Lock()
				keys = append(keys, key)
				mu.Unlock()
				t.Log("delete:", key)
			}
		}()

		// 等待订阅建立后发布，避免消息在订阅前发出而丢失
		waitReady(t, r)
		for _, key := range published {
			_ = r.Publish(key)
		}

		// 发布到其他 channel 的消息不应被本监听器收到
		for i := 'A'; i < 'Z'; i++ {
			rdb.Publish(context.TODO(), c2, i)
		}

		// 等待 10 条消息全部送达
		deadline := time.Now().Add(3 * time.Second)
		for {
			mu.Lock()
			n := len(keys)
			mu.Unlock()
			if n >= 10 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timeout: not all 10 published messages received")
			}
			time.Sleep(50 * time.Millisecond)
		}

		mu.Lock()
		defer mu.Unlock()

		// 断言：恰好收到 10 条，且内容与发布的 key 一一对应；
		// 其他 channel（c2）的消息（单字符 'A'-'Y'）若混入会使 Contains 失败。
		assert.Equal(t, 10, len(keys), "应恰好收到 10 条消息")
		for _, k := range keys {
			assert.Contains(t, published, k, "收到的消息内容应来自发布的 key 集合")
		}
	})
}

// TestCacheWatch 验证缓存失效链路：发布通知后，watcher 清除本地缓存。
func TestCacheWatch(t *testing.T) {
	channel := "abcdef"
	test.RunOnRedis(t, func(rdb redis.Client) {
		lis := NewListener(rdb, channel)
		defer func() { _ = lis.Close(context.Background()) }()

		c := cache.New(cache.WithMemStore(), cache.WithListener(lis))
		defer c.Close()

		key := "abc"

		// 写入并命中本地缓存
		assert.NoError(t, c.Put(context.Background(), key, "hello", 60))
		var s string
		assert.NoError(t, c.Get(context.Background(), key, &s))
		assert.Equal(t, "hello", s)

		// 发布失效通知前等待订阅建立（避免消息在订阅前发布而丢失，
		// 本地缓存将残留旧数据直到 TTL 过期）
		waitReady(t, lis)
		assert.NoError(t, lis.Publish(key))

		deadline := time.Now().Add(3 * time.Second)
		for {
			var s2 string
			err := c.Get(context.Background(), key, &s2)
			if errors.Is(err, cache.ErrEntityNotExist) {
				break // 本地缓存已被清除
			}
			if time.Now().After(deadline) {
				t.Fatal("local cache not invalidated after listener publish")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// TestListenerReconnection 验证基础投递链路：发布后订阅端能收到消息。
// 说明：断线重连路径依赖真实 Redis（健康检查 Ping 失败触发），
// 此处无法在本地模拟连接断开，仅验证消息投递链路正常。
func TestListenerReconnection(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		lis := NewListener(rdb, "test-reconnect-channel")
		defer func() { _ = lis.Close(context.Background()) }()

		// 等待订阅建立后再发布（at-most-once：订阅前的消息会丢失）
		waitReady(t, lis)
		err := lis.Publish("test-key")
		if err != nil {
			t.Fatalf("publish failed: %v", err)
		}

		select {
		case key := <-lis.Subscribe():
			assert.Equal(t, "test-key", key)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for published message")
		}
	})
}
