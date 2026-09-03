package leader

import (
	"context"
	"fmt"
	"time"
)

// ExampleElector 用 fakeLocker 演示一轮完整选举：竞选（先败后成）→
// 当选 → 续约 → 外层 ctx 取消 → 让位清理。
//
// 生产装配把 fakeLocker 换成真实的 lock 实例即可（后端在组合根选择）：
//
//	l := lock.New("svc:leader",
//		lock.WithBackend(redislock.New(rdb)), // 见 plugins/lock/redis
//		lock.WithTTL(15*time.Second),         // == WithLeaseDuration
//	)
//	e := leader.New(leader.WithLocker(l), leader.WithCallbacks(cb))
//
// 输出编排遵循确定性原则（评审 R4）：让位清理不等待业务 goroutine，
// 业务"观察到 ctx 取消"经 channel 回传主流程，由主流程按确定序统一打印。
func ExampleElector() {
	f := newFake()                     // renewOK 默认成功
	f.tryLockSeq = []bool{false, true} // 首次被他人持有，第二次当选
	observed := make(chan struct{})
	resume := make(chan struct{})
	f.unlockWait = resume // 门控清理：业务回传"已观察取消"后再放行 unlock/stopped

	var e *Elector
	e = New(WithLocker(f),
		WithIdentity("node-a"),
		WithLeaseDuration(200*time.Millisecond),
		WithRenewDeadline(120*time.Millisecond),
		WithRetryPeriod(20*time.Millisecond),
		WithCallbacks(Callbacks{
			OnStartedLeading: func(ctx context.Context, term uint64) {
				fmt.Println("started, term =", term, "identity =", e.Identity())
				<-ctx.Done()
				close(observed) // 取消事实回传主流程，不在此处打印
			},
			OnStoppedLeading: func() { fmt.Println("stopped") },
		}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	time.Sleep(50 * time.Millisecond) // 越过一次续约节拍（输出与此无关）
	cancel()                          // 优雅退出
	<-observed                        // 业务已确认观察到取消
	// 业务标记"已观察"并等待放行；Unlock/stopped/IsLeader 清除此刻尚未
	// 发生（unlock 被 resume 门控），读取值确定。
	fmt.Println("observed: leadership ended")
	close(resume) // 放行 unlock → stopped → Run 返回
	err := <-done
	fmt.Println("run returned:", err)
	fmt.Println("final term =", e.Term(), "isLeader =", e.IsLeader())

	// Output:
	// started, term = 1 identity = node-a
	// observed: leadership ended
	// stopped
	// run returned: context canceled
	// final term = 1 isLeader = false
}
