package leader

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// fakeLocker 实现 leader.Locker，全部行为可编程（对齐 lock/fake_test.go
// 的 fakeBackend 风格：内部测试包 + sync.Mutex + 注入字段）。
// 零值可用：TryLock 默认失败（他人持有）、Renew 默认成功、Unlock 默认成功。
type fakeLocker struct {
	mu sync.Mutex

	// TryLock 编程
	tryLockResult bool          // 恒定返回值（默认 false=他人持有）
	tryLockErr    error         // 非 nil 时返回 (result, err)——测 FailOpen 防御
	tryLockSeq    []bool        // 非空时按序消费（耗尽后重复末值）——测"先败后成"
	tryLockBlock  chan struct{} // 非 nil 时阻塞至关闭或 ctx 取消——测竞态时序
	tryLockHold   chan struct{} // 非 nil 时在返回前再等一层（同 ctx 可中断）——
	// 配合 tryLockHoldResult 编排"取消与 TryLock 返回交叠"的窗口（T8）
	tryLockHoldResult bool // true 时 hold 分支无视 ctx 取消，恒返回 (result, err)
	tryLockCalls      atomic.Int64

	// Renew 编程
	renewOK  bool  // 恒定 ok（置 false 测确认丢失；默认经 newFake 置 true）
	renewErr error // 恒定 err（可注入 lock.ErrRenewUnsupported /
	// fmt.Errorf("%w: x", lock.ErrBackendUnavailable)）
	renewOKAfterN   int           // 前 N 次失败（ok=false 或 renewErr）后恒完全成功——测抖动自愈
	renewFlakyArmed bool          // 内部：renewOKAfterN 曾被配置（恢复后忽略 renewErr）
	renewBlock      chan struct{} // 非 nil 时阻塞续约至关闭或 renewCtx 取消——测预算耗尽
	renewCalls      atomic.Int64
	renewLastTTL    atomic.Int64 // 最近一次 ttl 参数（ns）——断言 == LeaseDuration

	// Unlock 编程
	unlockErr   error
	unlockCalls atomic.Int64
	// unlockWait 非 nil 时，Unlock 在记录事件前等待其关闭（测试时序
	// 钩子：让业务回调先完成可观察动作再放行释放流程，确定事件序）。
	unlockWait chan struct{}

	// 事件日志（回调顺序不变量断言用）：互斥保护。
	events []string // "started"/"stopped"/"unlock"/"renew"/"tryLock"...
}

// Locker 接口契约核对（编译期）。
var _ Locker = (*fakeLocker)(nil)

func (f *fakeLocker) TryLock(ctx context.Context) (bool, error) {
	f.tryLockCalls.Add(1)
	f.record("tryLock")

	f.mu.Lock()
	block := f.tryLockBlock
	var result bool
	if len(f.tryLockSeq) > 0 {
		result = f.tryLockSeq[0]
		if len(f.tryLockSeq) > 1 {
			f.tryLockSeq = f.tryLockSeq[1:]
		}
	} else {
		result = f.tryLockResult
	}
	err := f.tryLockErr
	f.mu.Unlock()

	if err != nil {
		return result, err
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	f.mu.Lock()
	hold, holdResult := f.tryLockHold, f.tryLockHoldResult
	f.mu.Unlock()
	if hold != nil {
		if holdResult {
			<-hold // 无视取消：确定性以 (result, err) 返回
		} else {
			select {
			case <-hold:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
	}
	return result, nil
}

// newFake 返回"默认健康"的 fake：TryLock 失败（他人持有）、Renew 成功、
// Unlock 成功。注意 renewOK 的零值 false 故意表示确认丢失，需要默认成功
// 续约的场景必须经本函数构造（或显式 renewOK: true）。
func newFake() *fakeLocker {
	return &fakeLocker{renewOK: true}
}

func (f *fakeLocker) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	f.renewCalls.Add(1)
	f.renewLastTTL.Store(int64(ttl))
	f.record("renew")

	f.mu.Lock()
	block := f.renewBlock
	n := f.renewOKAfterN
	ok := f.renewOK
	err := f.renewErr
	limit := f.renewFlakyArmed
	if n > 0 {
		f.renewOKAfterN = n - 1
		f.renewFlakyArmed = true
	}
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return false, ctx.Err() // 模拟后端挂起、被预算超时打断
		}
	}
	if n > 0 {
		// 前 N 次失败：注入 err 则按瞬时故障形态 (false, err)，
		// 否则 (false, nil)。
		if err != nil {
			return false, err
		}
		return false, nil
	}
	if limit {
		// 抖动自愈剧本（renewOKAfterN 曾配置）：恢复后完全成功，
		// 忽略 renewErr——(true, err) 的 FailOpen 组合另行用
		// renewOK+renewErr 常量字段表达（T13）。
		return true, nil
	}
	return ok, err
}

func (f *fakeLocker) Unlock(ctx context.Context) error {
	f.unlockCalls.Add(1)

	f.mu.Lock()
	wait, err := f.unlockWait, f.unlockErr
	f.mu.Unlock()

	if wait != nil {
		<-wait // 无 ctx 监听：仅测试编排用，主测试保证最终放行
	}
	f.record("unlock")
	return err
}

// record 追加一条事件日志（互斥保护，可在任意 goroutine 调用）。
func (f *fakeLocker) record(ev string) {
	f.mu.Lock()
	f.events = append(f.events, ev)
	f.mu.Unlock()
}

// eventSeq 返回事件日志快照。
func (f *fakeLocker) eventSeq() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// eventCount 统计某类事件的出现次数。
func (f *fakeLocker) eventCount(ev string) int {
	n := 0
	for _, e := range f.eventSeq() {
		if e == ev {
			n++
		}
	}
	return n
}
