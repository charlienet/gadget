package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// frozenClock 是恒定时间源：让 Runnable Example 的输出与真实耗时无关，
// 完全确定（演示用法时即"测试免 sleep"的 WithClock 注入形态）。
type frozenClock struct{}

func (frozenClock) Now() time.Time { return time.Unix(1_700_000_000, 0) }

// ExampleLimiter_Allow 演示 memory 后端的单机限流：速率 5/1s、突发 5。
// 时钟冻结在初始满桶时刻，总放行量恰等于桶量（租约批发不改变全局速率）。
func ExampleLimiter_Allow() {
	limiter := New(Memory(),
		WithClock(frozenClock{}),
		WithRate(5, time.Second),
		WithBurst(5),
	)
	defer limiter.Close()

	ctx := context.Background()
	for i := 0; i < 7; i++ {
		ok, err := limiter.Allow(ctx, "user:42", 1)
		if err != nil && !errors.Is(err, ErrExceeded) {
			fmt.Println("unexpected error:", err)
			return
		}
		fmt.Println(ok)
	}

	// Output:
	// true
	// true
	// true
	// true
	// true
	// false
	// false
}

// batchBackend 是演示用批发后端：每次按请求量全额授予，并统计调用次数。
type batchBackend struct {
	mu    sync.Mutex
	calls int
}

func (b *batchBackend) Wholesale(_ context.Context, _ string, want int, _ Spec, _ GrantMode) (int, time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return want, 0, nil
}

func (b *batchBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// ExampleLimiter_wholesaleMerge 演示租约模式的 in-flight 批发合并：
// 10 个并发请求冷启动，同 key 只产生一次后端批发调用（leader 批发、
// followers 共享结果；后续请求命中注入后的本地存量）。
func ExampleLimiter_wholesaleMerge() {
	backend := &batchBackend{}
	limiter := New(backend,
		WithRate(100, time.Second), // want = round(100×1s/1s)×0.5 = 50，足以覆盖 10 个并发
		WithBurst(200),
	)
	defer limiter.Close()

	var wg sync.WaitGroup
	passes := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _ := limiter.Allow(context.Background(), "api", 1)
			passes <- ok
		}()
	}
	wg.Wait()
	close(passes)

	allowed := 0
	for ok := range passes {
		if ok {
			allowed++
		}
	}
	fmt.Printf("passed: %d\nwholesale calls: %d\n", allowed, backend.count())

	// Output:
	// passed: 10
	// wholesale calls: 1
}

// ExampleLimiter_Wait 演示 Wait 的 WithMaxWait 出口：桶耗尽后阻塞等待，
// 总等待超出上限时返回 ErrExceeded 语义错误（而非无限挂起）。
func ExampleLimiter_Wait() {
	limiter := New(Memory(),
		WithRate(5, time.Second),
		WithBurst(5),
		WithMaxWait(50*time.Millisecond),
	)
	defer limiter.Close()

	ctx := context.Background()
	// 先耗尽初始满桶的 5 个令牌。
	for i := 0; i < 5; i++ {
		if _, err := limiter.Allow(ctx, "burst", 1); err != nil {
			fmt.Println("drain failed:", err)
			return
		}
	}

	// 继续请求：令牌回补需要时间，等待总量被 WithMaxWait 截断。
	err := limiter.Wait(ctx, "burst", 1)
	fmt.Println("wait exceeded:", errors.Is(err, ErrExceeded))

	// Output:
	// wait exceeded: true
}
