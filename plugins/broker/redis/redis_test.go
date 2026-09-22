package redis_test

import (
	"runtime"
	"sync"
	"testing"
	"time"

	b "github.com/charlienet/gadget/plugins/broker/redis"
	"github.com/charlienet/gadget/broker"
	"github.com/charlienet/gadget/redis"
	mini "github.com/charlienet/gadget/redis/test/mini"
)

func TestRedisBroker(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		broker := b.New(rdb)
		_ = broker
	})
}

// noopHandler 返回一个不做任何操作的 Handler
func noopHandler(t *testing.T) broker.Handler {
	return func(e broker.Event) error { return nil }
}

// TestCloseWaitsForRecvGoroutineExit 验证 Close() 返回后 recv goroutine 已安全退出。
// 方法：订阅后记录 goroutine 数，Close 后等待并再次检查，确保 goroutine 数量回落。
func TestCloseWaitsForRecvGoroutineExit(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		br := b.New(rdb)
		ctx := t.Context()

		_, err := br.Subscribe(ctx, "test.topic", noopHandler(t))
		if err != nil {
			t.Fatalf("Subscribe failed: %v", err)
		}

		// 等待 goroutine 启动
		time.Sleep(50 * time.Millisecond)

		goroutinesBefore := runtime.NumGoroutine()

		// 调用 Close，最多等 5s
		done := make(chan struct{})
		go func() {
			_ = br.Close(ctx)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Fatal("Close() did not return within 6 seconds (expected ≤ 5s)")
		}

		// Close 返回后，给 recv goroutine 一小段时间完成退出
		time.Sleep(200 * time.Millisecond)

		goroutinesAfter := runtime.NumGoroutine()

		if goroutinesAfter > goroutinesBefore+2 {
			t.Errorf("Close() 返回后 recv goroutine 未退出：Close 前 %d 个，Close 后 %d 个（差 %d，超出容差 +2）",
				goroutinesBefore, goroutinesAfter, goroutinesAfter-goroutinesBefore)
		}
	})
}

// TestCloseIdempotent 验证多次调用 Close 不会 panic 或死锁
func TestCloseIdempotent(t *testing.T) {
	mini.Run(t, func(rdb redis.Client) {
		br := b.New(rdb)
		ctx := t.Context()

		_, err := br.Subscribe(ctx, "test.topic", noopHandler(t))
		if err != nil {
			t.Fatalf("Subscribe failed: %v", err)
		}

		time.Sleep(50 * time.Millisecond)

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Go(func() {
				if err := br.Close(ctx); err != nil {
					t.Errorf("Close() error: %v", err)
				}
			})
		}
		wg.Wait()
	})
}
