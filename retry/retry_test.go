package retry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// withFakeSleep 将 sleepFunc 替换为记录器并返回记录指针；t.Cleanup 还原。
// cost 为每次 sleep 的真实耗时（如 time.Millisecond），用于让 MaxElapsed
// 检查确定化；零等待测试传 0。记录的是 Do clamp 之后（d>=0）请求的时长。
// 覆写 sleepFunc 的测试禁止 t.Parallel()。
func withFakeSleep(t *testing.T, cost time.Duration) *[]time.Duration {
	t.Helper()
	old := sleepFunc
	rec := &[]time.Duration{}
	sleepFunc = func(ctx context.Context, d time.Duration) error {
		*rec = append(*rec, d)
		if cost > 0 {
			time.Sleep(cost)
		}
		// 与 defaultSleep 对齐：先检查 ctx，已取消则中断。
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
	t.Cleanup(func() { sleepFunc = old })
	return rec
}

// cancelledSleep 返回替换 sleepFunc 的设置函数：模拟睡眠中 ctx 被取消。
func cancelledSleep() func(context.Context, time.Duration) error {
	return func(context.Context, time.Duration) error {
		return context.Canceled
	}
}

func TestDoFirstSuccess(t *testing.T) {
	calls := 0
	err := Do(context.Background(), func(context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestDoSuccessOnNthAttempt(t *testing.T) {
	sleeps := withFakeSleep(t, 0)
	calls := 0
	wantErr := errors.New("boom")
	err := Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return wantErr
		}
		return nil
	}, WithBackoff(Fixed(7*time.Millisecond)), WithMaxAttempts(5))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if len(*sleeps) != 2 || (*sleeps)[0] != 7*time.Millisecond {
		t.Fatalf("sleeps = %v, want [7ms 7ms]", *sleeps)
	}
}

func TestDoInterAttemptCtxCheckBranch(t *testing.T) {
	// 直测 Do 尝试之间的 ctx 检查分支（retry.go :114-116）：
	// fake sleep 恒返回 nil（模拟未观察到的取消），fn 在第二次执行中
	// 取消 ctx 并返回可重试错误；回到循环后 :108 睡眠放行、:114 命中
	// ctx.Err() 返回——规则 3 的「尝试之间」路径，独立于睡眠中断路径。
	old := sleepFunc
	sleepFunc = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { sleepFunc = old })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := Do(ctx, func(context.Context) error {
		calls++
		if calls == 2 {
			cancel() // 取消发生于 fn 内；fake sleep 不感知
		}
		return errors.New("boom")
	}, WithMaxAttempts(100), WithBackoff(Fixed(time.Millisecond)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled（尝试间检查命中）", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestDoNonRetryableReturnsImmediately(t *testing.T) {
	calls := 0
	sentinel := errors.New("permanent")
	err := Do(context.Background(), func(context.Context) error {
		calls++
		return fmt.Errorf("wrap: %w", sentinel)
	}, WithRetryable(func(error) bool { return false }))
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 包装 %v", err, sentinel)
	}
	if err.Error() != "wrap: permanent" {
		t.Fatalf("err = %q, want 原样返回不加工", err.Error())
	}
}

func TestDoAttemptsExhaustedReturnsLastErr(t *testing.T) {
	withFakeSleep(t, 0)
	calls := 0
	errs := []error{errors.New("e1"), errors.New("e2"), errors.New("e3")}
	err := Do(context.Background(), func(context.Context) error {
		defer func() { calls++ }()
		return errs[calls]
	}, WithMaxAttempts(3), WithBackoff(Fixed(time.Millisecond)))
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if err != errs[2] {
		t.Fatalf("err = %v, want 最后一次原始错误 %v", err, errs[2])
	}
	// 原始错误保真：不包装，errors.Is 按指针即命中
	if !errors.Is(err, errs[2]) {
		t.Fatal("errors.Is 对原始错误失配")
	}
}

func TestDoDefaultAttemptsIsFive(t *testing.T) {
	withFakeSleep(t, 0)
	calls := 0
	err := Do(context.Background(), func(context.Context) error {
		calls++
		return errors.New("always")
	})
	if calls != 5 {
		t.Fatalf("默认配置 calls = %d, want 5", calls)
	}
	if err == nil {
		t.Fatal("默认配置耗尽应返回最后错误")
	}
}

func TestDoSleepCancelledReturnsCtxErr(t *testing.T) {
	old := sleepFunc
	sleepFunc = cancelledSleep()
	t.Cleanup(func() { sleepFunc = old })

	calls := 0
	err := Do(context.Background(), func(context.Context) error {
		calls++
		return errors.New("boom")
	}, WithMaxAttempts(10))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1（首次失败后睡眠被取消）", calls)
	}
}

func TestDoSuccessWinsOverCancelledCtx(t *testing.T) {
	// 规则 1：fn 返回 nil → 即使 ctx 已取消也返回成功。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Do(ctx, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("err = %v, want nil（成功优先于 ctx 取消）", err)
	}
}

func TestDoCanceledCtxAfterFailedFnReturnsLastErr(t *testing.T) {
	// 规则 1/2 与规则 3 的优先级：fn 返回时（无论取消是否发生于 fn 内），
	// nil→成功优先；不可重试错误→原始错误优先于 ctx.Err()。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sentinel := errors.New("sentinel")
	err := Do(ctx, func(context.Context) error { return sentinel },
		WithRetryable(func(error) bool { return false }))
	if err != sentinel {
		t.Fatalf("err = %v, want %v（不可重试优先返回原始错误）", err, sentinel)
	}
}

func TestDoFnSeesLiveCtx(t *testing.T) {
	// 规则 4：fn 执行中 ctx 取消不中断 fn，fn 可感知取消并自行返回，
	// 其错误按规则 2 原样返回（此时不做尝试间 ctx 检查）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sentinel := errors.New("fn-self-decided")
	gotLive := false
	err := Do(ctx, func(context.Context) error {
		select {
		case <-ctx.Done():
			gotLive = false
		default:
			gotLive = true // ctx 进入 fn 时仍存活
		}
		cancel() // fn 执行中取消
		return sentinel
	}, WithRetryable(func(error) bool { return false }))
	if !gotLive {
		t.Fatal("fn 执行中 ctx 不应已取消")
	}
	if err != sentinel {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestDoAttemptsWinsBeforeElapsed(t *testing.T) {
	sleeps := withFakeSleep(t, 0)
	calls := 0
	last := errors.New("last")
	err := Do(context.Background(), func(context.Context) error {
		calls++
		return last
	}, WithMaxAttempts(2), WithMaxElapsed(time.Hour), WithBackoff(Fixed(time.Second)))
	if calls != 2 || err != last {
		t.Fatalf("calls=%d err=%v, want 2/%v（次数先到）", calls, err, last)
	}
	if len(*sleeps) != 1 {
		t.Fatalf("sleeps=%v, want 1 次", *sleeps)
	}
}

func TestDoElapsedWinsBeforeAttempts(t *testing.T) {
	// fake sleep + 确定性 30ms 耗时（cost）：时间先到终止，远早于次数
	// 上限。时序：第 2 轮检查 30ms<50ms 放行，第 3 轮 60ms>=50ms 终止
	// （calls=2），两侧余量 10ms 不依赖调度抖动；并断言 sleep 记录。
	sleeps := withFakeSleep(t, 30*time.Millisecond)
	calls := 0
	e1, e2 := errors.New("e1"), errors.New("e2")
	err := Do(context.Background(), func(context.Context) error {
		calls++
		if calls == 1 {
			return e1
		}
		return e2
	}, WithMaxAttempts(100), WithMaxElapsed(50*time.Millisecond), WithBackoff(Fixed(30*time.Millisecond)))
	if calls != 2 {
		t.Fatalf("calls = %d, want 2（时限先到，远早于 100 次）", calls)
	}
	if len(*sleeps) != 2 || (*sleeps)[0] != 30*time.Millisecond || (*sleeps)[1] != 30*time.Millisecond {
		t.Fatalf("sleeps = %v, want [30ms 30ms]（calls=2 之间 1 次 + 末轮睡醒后被时限拦截 1 次）", *sleeps)
	}
	if err != e2 {
		t.Fatalf("err = %v, want %v", err, e2)
	}
}

func TestDoElapsedIsSoftLimit(t *testing.T) {
	// 软上限语义：睡眠不被截断——退避请求 200ms 远超预算 30ms，
	// sleepFunc 仍收到完整 200ms（不是剩余时间）；睡醒后时限检查
	// 立即终止并返回最后错误。"实际耗时可超出 MaxElapsed 至多一个
	// 退避间隔"即由本场景保证。
	sleeps := withFakeSleep(t, time.Millisecond)
	calls := 0
	e := errors.New("x")
	err := Do(context.Background(), func(context.Context) error {
		calls++
		return e
	}, WithMaxAttempts(100), WithMaxElapsed(500*time.Microsecond), WithBackoff(Fixed(200*time.Millisecond)))
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != 200*time.Millisecond {
		t.Fatalf("sleeps = %v, want [200ms]（睡眠未截断为剩余预算）", *sleeps)
	}
	if err != e {
		t.Fatalf("err = %v, want %v", err, e)
	}
}

func TestDoPanicPropagates(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("fn panic 应穿透 Do，不得 recover")
		}
		if r != "boom-panic" {
			t.Fatalf("panic 值 = %v, want boom-panic", r)
		}
	}()
	_ = Do(context.Background(), func(context.Context) error {
		panic("boom-panic")
	})
}

func TestDoNonPositiveBackoffMeansImmediateRetry(t *testing.T) {
	sleeps := withFakeSleep(t, 0)
	calls := 0
	e := errors.New("x")
	// 自定义 Backoff 返回 0/负值：Do 内 clamp 为 0 → 立即重试，
	// 每次仍经过 sleep 路径（给 ctx 检查机会）。
	err := Do(context.Background(), func(context.Context) error {
		calls++
		return e
	}, WithMaxAttempts(3), WithBackoff(&seqBackoff{values: []time.Duration{0, -5, 1, 1}}))
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if err != e {
		t.Fatalf("err = %v, want %v", err, e)
	}
	want := []time.Duration{0, 0} // 0 与 -5 均被 clamp 为 0
	if fmt.Sprint(*sleeps) != fmt.Sprint(want) {
		t.Fatalf("sleeps = %v, want %v（非正值视为 0，立即重试）", *sleeps, want)
	}
}

func TestDoSharedBackoffResetEachCall(t *testing.T) {
	withFakeSleep(t, 0)
	b := Exponential(100*time.Millisecond, 2, 10*time.Second)
	failOnce := func(context.Context) error { return errors.New("x") }
	noop := func(context.Context) error { return nil }

	// 顺序两次 Do（各失败一次触发一次退避），中间穿插一次成功 Do
	// 消耗底层序列；第二次 Do 首步仍应为 100ms（Reset 生效）。
	_ = Do(context.Background(), failOnce, WithBackoff(b), WithMaxAttempts(2))
	_ = Do(context.Background(), noop, WithBackoff(b))

	old := sleepFunc
	sleepFunc = func(ctx context.Context, d time.Duration) error {
		if d != 100*time.Millisecond {
			t.Errorf("第二次 Do 退避 = %v, want 100ms（序列从头）", d)
		}
		return old(ctx, d)
	}
	_ = Do(context.Background(), failOnce, WithBackoff(b), WithMaxAttempts(2))
}

func TestOptionsIgnoreNilAndInvalid(t *testing.T) {
	o := defaultOptions()
	WithBackoff(nil)(o)
	WithMaxAttempts(0)(o)
	WithMaxAttempts(-3)(o)
	WithMaxElapsed(0)(o)
	WithMaxElapsed(-time.Second)(o)
	WithRetryable(nil)(o)

	if o.maxAttempts != 5 {
		t.Fatalf("maxAttempts = %d, want 默认 5", o.maxAttempts)
	}
	if o.maxElapsed != 0 {
		t.Fatalf("maxElapsed = %v, want 0（禁用）", o.maxElapsed)
	}
	if !o.retryable(errors.New("any")) {
		t.Fatal("retryable 应保持默认（全部可重试）")
	}
	if b := o.backoff(); b == nil || b.NextBackOff() != 100*time.Millisecond {
		t.Fatal("backoff 应保持默认指数策略")
	}
}

func TestDoNilOptionIgnored(t *testing.T) {
	withFakeSleep(t, 0)
	calls := 0
	e := errors.New("x")
	err := Do(context.Background(), func(context.Context) error {
		calls++
		return e
	}, nil, WithMaxAttempts(2))
	if calls != 2 || err != e {
		t.Fatalf("calls=%d err=%v, want 2/%v（nil Option 不生效不 panic）", calls, err, e)
	}
}

func TestDoConcurrentDefaultConfigRace(t *testing.T) {
	// go test -race 下验证：backoff 工厂在每次 Do 内创建独立实例，
	// 并发 Do（含默认配置）零数据竞争。
	// 使用无共享状态的 fake sleep（只读 ctx），避免记录器本身引入 race。
	old := sleepFunc
	sleepFunc = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { sleepFunc = old })
	const n = 16
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errCh <- fmt.Errorf("goroutine %d panic: %v", i, r)
				}
			}()
			var err error
			if i%2 == 0 {
				// 默认配置（共享工厂闭包 + 每 Do 新建 Exponential 实例）
				calls := 0
				err = Do(context.Background(), func(context.Context) error {
					calls++
					if calls < 3 {
						return errors.New("transient")
					}
					return nil
				})
			} else {
				calls := 0
				err = Do(context.Background(), func(context.Context) error {
					calls++
					if calls < 3 {
						return errors.New("transient")
					}
					return nil
				}, WithBackoff(Fixed(time.Duration(i)*time.Millisecond)))
			}
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("并发 Do 出错: %v", err)
		}
	}
}

func TestDefaultSleepInterruptedByCtx(t *testing.T) {
	// 生产路径 defaultSleep 本体：长睡眠期间 ctx 取消应立即返回
	// ctx.Err()（time.Timer + select，禁止 time.Sleep 语义）。
	ctx, cancel := context.WithCancel(context.Background())
	old := sleepFunc
	sleepFunc = defaultSleep
	t.Cleanup(func() { sleepFunc = old })

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := sleepFunc(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("耗时 %v，未被 ctx 及时中断", elapsed)
	}
}

// 以下两个测试直接调用 defaultSleep 本体（不覆写 sleepFunc），
// 与其他测试并行安全。

func TestDefaultSleepNonPositiveReturnsCtxErr(t *testing.T) {
	t.Parallel()
	// retry.go defaultSleep d<=0 分支：零等待也如实反馈 ctx 状态。
	if err := defaultSleep(context.Background(), 0); err != nil {
		t.Fatalf("存活 ctx d=0 = %v, want nil", err)
	}
	if err := defaultSleep(context.Background(), -time.Second); err != nil {
		t.Fatalf("存活 ctx d<0 = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := defaultSleep(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 ctx d=0 = %v, want context.Canceled", err)
	}
	if err := defaultSleep(ctx, -time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 ctx d<0 = %v, want context.Canceled", err)
	}
}

func TestDefaultSleepZeroWaitStillChecksTimeout(t *testing.T) {
	t.Parallel()
	// d<=0 快速路径同样覆盖超时态：返回 DeadlineExceeded 而非 nil。
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done() // 等待超时落地，确定化
	if err := defaultSleep(ctx, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时 ctx d=0 = %v, want context.DeadlineExceeded", err)
	}
}
