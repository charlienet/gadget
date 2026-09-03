// Package leader 提供基于分布式锁的 leader 选举状态机。
//
// 设计借鉴 client-go leaderelection 的三参数模型（LeaseDuration /
// RenewDeadline / RetryPeriod），但互斥裁决完全委托给注入的 Locker
// （*lock.Lock 天然满足）：leader 只做选举状态机，不感知后端。
// Redis/etcd 等后端的装配发生在业务组合根：
//
//	l := lock.New("svc:leader",
//		lock.WithBackend(redislock.New(rdb)), // 组合根选择后端
//		lock.WithTTL(15*time.Second),         // 应 == WithLeaseDuration
//	)
//	e := leader.New(
//		leader.WithLocker(l),
//		leader.WithCallbacks(leader.Callbacks{
//			OnStartedLeading: func(ctx context.Context, term uint64) { run(ctx, term) },
//			OnStoppedLeading: func() { cleanup() },
//		}),
//	)
//	if err := e.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
//		log.Printf("选举结束: %v", err)
//	}
//
// 任期（term）与 fencing 边界：每次成功当选 term 单调递增，并作为参数
// 传入 OnStartedLeading，供业务侧识别/拒绝旧任期的僵尸回调。注意 term 仅
// 在单进程内有 fencing 意义；跨进程强 fencing 需要存储侧单调版本号，
// lock 抽象当前不提供，本模块亦不提供。旧 leader 进程冻结/复活场景的
// 双写窗口只能靠租约时序压缩（约 ≤ 1.25×RetryPeriod + 一次续约调用
// 耗时：恢复后首次续约返回确认丢失即让位），无法根除；需要强 fencing
// 的业务须在存储侧自行校验版本号单调性。
//
// 时钟约束建议：LeaseDuration - RenewDeadline（默认 5s 余量）应 ≥ 预估
// 时钟偏差 + 最大 GC/调度停顿；锁的过期由存储侧 TTL 裁决，本地钟只影响
// 续约时机。
//
// 注入的 lock 实例应使用默认 FailClosed 策略：leader 对 "err 非 nil" 的
// 返回值一律不信任其 ok 分量，FailOpen 的放行值会被忽略（防御误配双主）。
// 后端必须支持续期（lock.Backend 实现 lock.Renewer），否则 Run 在首次
// 续约时以 lock.ErrRenewUnsupported 终止。一个 Elector 独占一个 lock
// 实例，禁止将同一 *lock.Lock 注入多个并发运行的 Elector。
package leader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	mrand "math/rand"

	"github.com/charlienet/gadget/lock"
)

// 默认参数（与 client-go leaderelection 默认一致）。
const (
	defaultLeaseDuration = 15 * time.Second
	defaultRenewDeadline = 10 * time.Second
	defaultRetryPeriod   = 2 * time.Second

	// releaseTimeout 是让位清理中 Unlock 的尽力而为超时：原 ctx 此时
	// 通常已取消，用脱离取消传播的独立 ctx + 本超时兜底，避免锁残留
	// 至 TTL 拖慢其他竞选者接管。
	releaseTimeout = 5 * time.Second
)

// Locker 是选举所需的最小锁能力，*lock.Lock 天然满足（编译期断言见下）。
// leader 仅依赖本接口，锁的构造（key、TTL、后端、FailPolicy）由业务
// 组合根完成。
//
// 实现契约（与 lock.Lock 行为一致）：
//   - TryLock：单次尝试。 (true, nil) 获锁成功；(false, nil) 被他人持有；
//     err 非 nil 表示后端故障（可能包装 lock.ErrBackendUnavailable）。
//     err 非 nil 时 leader 不信任 ok 分量（防御 FailOpen 误配双主）。
//   - Renew：将租约延长至 ttl。(false, nil) 表示锁已确认丢失（过期或被
//     他人获取），leader 据此立即让位；err 非 nil 为瞬时故障，进入
//     RenewDeadline 预算倒计时。
//   - Unlock：释放锁；token 不匹配时静默返回 nil（无副作用）。
type Locker interface {
	TryLock(ctx context.Context) (bool, error)
	Unlock(ctx context.Context) error
	Renew(ctx context.Context, ttl time.Duration) (bool, error)
}

// 编译期断言：*lock.Lock 满足 Locker（对齐仓库惯例，如
// plugins/lock/redis/redis.go:39-40）。
var _ Locker = (*lock.Lock)(nil)

// ErrLeadershipLost 表示任期因续约失败/锁丢失而终止（非调用方主动取消）。
// Run 返回的错误可用 errors.Is 判定；丢失原因（最后一次续约错误，或
// "renew confirmed lock lost" 固定文案）以 "%w: %v" 形式附加在消息中。
var ErrLeadershipLost = errors.New("leader: leadership lost")

// Callbacks 是选举生命周期回调。除 OnStartedLeading 外均可选（nil 跳过）。
//
// 调用约定：
//   - 所有回调都在 Run 调用者的控制流之外或末尾执行（OnStartedLeading 在
//     独立 goroutine，OnStoppedLeading 在 Run 的 goroutine 同步调用），
//     必须快速返回，不得阻塞（长驻业务逻辑应在 OnStartedLeading 内部
//     自行派生 goroutine）；
//   - 回调 panic 不做 recover，直接穿透（与 retry.Do 对 fn panic 的约定
//     一致）：OnStartedLeading panic 使进程崩溃；OnStoppedLeading panic
//     穿透出 Run，该轮 stepDown 不完整（锁交 TTL 自然过期）；
//   - OnStartedLeading 必须响应 ctx 取消并及时返回：leader 不等待其
//     退出即继续让位流程（与 client-go 一致），业务响应取消的时延构成
//     理论双写窗口，需以 term 贯通写路径缓解。
type Callbacks struct {
	// OnStartedLeading 在当选后以独立 goroutine 调用（每轮 Run 至多一次，
	// 且只在当选确认后触发）。ctx 在让位时（优雅退出/丢失/致命错误）被
	// 取消；term 为本轮任期号，业务侧应将其贯通到写路径日志与僵尸回调
	// 判别——判别新旧必须用本参数（定格于调用瞬间），不得在回调中途
	// 重读 Elector.Term()。返回即视为主动让位（resigned），Run 随后
	// 返回 nil。
	OnStartedLeading func(ctx context.Context, term uint64)

	// OnStoppedLeading 在每轮成功当选的让位流程末尾同步调用，恰好一次，
	// 且发生在业务 ctx 取消与锁释放（尽力而为）之后、Run 返回之前。
	// 未当选过的 Run（acquire 阶段取消、当选瞬间取消）不触发——与
	// OnStartedLeading 严格成对。
	OnStoppedLeading func()
}

// Elector 是 leader 选举状态机。零值不可用，必须经 New 构造。
//
// 并发契约：Run 不支持并发调用（第二次并发 Run panic）；同一 Elector
// 可串行多次 Run（每轮 term 递增），常见用法是外层 for 循环实现
// "让位后自动重新竞选"。IsLeader/Term/Identity 可被任意 goroutine
// 并发调用。一个 Elector 独占一个 Locker 实例。
type Elector struct {
	locker        Locker
	identity      string
	leaseDuration time.Duration
	renewDeadline time.Duration
	retryPeriod   time.Duration
	callbacks     Callbacks

	isLeader atomic.Bool
	term     atomic.Uint64
	running  atomic.Bool // Run 并发防护（CAS）
}

// New 创建 Elector。以下情形 panic（构造期编程错误 fail-fast，与
// lock.New/retry.Fixed 惯例一致）：
//   - 未通过 WithLocker 注入 Locker；
//   - Callbacks.OnStartedLeading 为 nil；
//   - 参数不满足 LeaseDuration > RenewDeadline > RetryPeriod > 0。
//
// 默认值：LeaseDuration 15s、RenewDeadline 10s、RetryPeriod 2s
// （与 client-go 默认一致）、identity 为 "hostname-pid"。identity 仅
// 用于日志/观测自描述：受 lock 抽象限制（无持有者探查能力），本模块
// 无法感知其他竞选者的身份。
func New(opts ...Option) *Elector {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	if o.locker == nil {
		panic("leader: WithLocker 未注入 Locker")
	}
	if o.callbacks.OnStartedLeading == nil {
		panic("leader: Callbacks.OnStartedLeading 不能为 nil")
	}
	if !(o.leaseDuration > o.renewDeadline && o.renewDeadline > o.retryPeriod && o.retryPeriod > 0) {
		panic(fmt.Sprintf("leader: 参数必须满足 LeaseDuration(%v) > RenewDeadline(%v) > RetryPeriod(%v) > 0",
			o.leaseDuration, o.renewDeadline, o.retryPeriod))
	}
	return &Elector{
		locker:        o.locker,
		identity:      o.identity,
		leaseDuration: o.leaseDuration,
		renewDeadline: o.renewDeadline,
		retryPeriod:   o.retryPeriod,
		callbacks:     o.callbacks,
	}
}

// Run 执行一轮完整选举：阻塞竞选 → 当选领导 → 让位返回。
//
// 返回语义：
//   - 竞选阶段 ctx 取消/超时（未当选）→ ctx.Err()，不触发任何回调；
//   - 当选瞬间检出 ctx 已取消 → 尽力 Unlock 后返回 ctx.Err()，同样
//     零回调、零 term 消耗；
//   - 当选后 ctx 取消（优雅退出）→ 取消业务 ctx、释放锁、触发
//     OnStoppedLeading 后返回 ctx.Err()；
//   - 当选后锁丢失/续约预算耗尽 → 同上清理后返回 ErrLeadershipLost
//     （errors.Is 可判定，消息附最后一次续约错误）；
//   - OnStartedLeading 自行返回（主动让位）→ 同上清理后返回 nil；
//   - 后端不支持续约 → 同上清理后返回 errors.Join(ErrLeadershipLost,
//     err)，errors.Is 同时命中 lock.ErrRenewUnsupported。
//
// 多让位信号同时就绪（如 ctx 取消与续约失败同瞬）时 select 随机选取，
// Run 返回值的分类边界存在二义，但均为合法让位，回调成对不变量不受影响。
//
// 锁释放为尽力而为（以 context.Background 为父脱离取消传播 + 独立 5s
// 超时，见 releaseTimeout），失败不影响返回值——锁最迟在
// LeaseDuration 后自然过期（自最后一次成功续约起算）。
//
// 注意：若让位清理中的回调（OnStoppedLeading）panic 穿透，stepDown
// 顺序保证此时锁已释放、业务 ctx 已取消，残渣仅为本应执行的
// OnStoppedLeading 未执行；running 与 isLeader 标志均经 defer 复位，
// Elector 可再次 Run，读数不受影响。
func (e *Elector) Run(ctx context.Context) error {
	if !e.running.CompareAndSwap(false, true) {
		panic("leader: Elector 不支持并发 Run")
	}
	defer e.running.Store(false)

	if err := e.acquire(ctx); err != nil {
		return err // 竞选放弃：零回调
	}

	// 当选确认序列（顺序固定，勿调换）：
	// 第一步非阻塞检查 ctx——TryLock 期间 ctx 可能已取消。已取消则
	// 尽力释放刚获得的锁并返回，零回调、零 term 消耗（never started,
	// never stopped），不做"启动后立即停止"的抖动回调。
	if err := ctx.Err(); err != nil {
		e.releaseLock()
		return err
	}
	term := e.term.Add(1)
	e.isLeader.Store(true)
	defer e.isLeader.Store(false) // 正常路径 stepDown 已清，此处兜底 panic 泄漏

	leadCtx, cancelLead := context.WithCancel(ctx)
	defer cancelLead() // 幂等兜底：用户 Locker 实现 panic 穿透 stepDown
	// 时，封堵业务 goroutine 永久挂起在 leadCtx.Done 上的泄漏窗口。
	startedDone := make(chan struct{})
	go func() {
		defer close(startedDone)
		e.callbacks.OnStartedLeading(leadCtx, term)
	}()

	err := e.renew(leadCtx, startedDone)
	e.stepDown(cancelLead)
	if err != nil {
		// 丢失/优雅退出：保留 renew 给出的原因（含 errors.Join 的
		// 多错误链），消息记录在清理之后、返回之前。
		return err
	}
	// err==nil：renew 只因业务主动让位（startedDone）或优雅退出返回
	// nil；优雅退出的 ctx.Done case 直接携带 ctx.Err()（err!=nil），
	// 故此处为 resigned——若外层 ctx 恰已取消则保守返回 ctx.Err()
	// （S3 分类边界二义时取取消侧），外层存活时返回 nil。
	return ctx.Err()
}

// IsLeader 返回当前是否处于领导状态（当选确认后置位，让位决策时清除）。
// 反映的是状态机决策，可能略先于回调触达。回调 panic 穿透时 defer
// 兜底清除，Run 返回后本读数必为 false，不会滞留。
func (e *Elector) IsLeader() bool { return e.isLeader.Load() }

// Term 返回当前任期号（最近一次成功当选的 term；从未当选为 0）。
// 注意与 OnStartedLeading 收到的 term 参数的区别：回调参数定格于调用
// 瞬间，本方法返回"当前值"，重新竞选后会变化——业务写路径判别僵尸
// 回调必须使用回调参数，不得在回调中途重读本方法。
func (e *Elector) Term() uint64 { return e.term.Load() }

// Identity 返回本节点竞选身份（默认 "hostname-pid"）。仅用于日志/观测
// 自描述：受 lock 抽象限制（无持有者探查能力），本模块无法感知他人身份。
func (e *Elector) Identity() string { return e.identity }

// acquire 阻塞竞选直到获锁或 ctx 取消。返回 nil 表示当选（调用方随即
// 执行当选确认）；ctx.Err() 表示放弃。TryLock 的 err（含
// lock.ErrBackendUnavailable）不终止竞选，静默按 nextRetry(RetryPeriod)
// 轮询直到 ctx 取消（leader 无 logger 依赖，不做日志）。
//
// err != nil 时无论 ok 为何值一律不信任 ok（防御 FailOpen 误配：
// lock 的 FailOpen 兜底会产生 (true, 非nil err) 组合，若信任 ok 将
// 导致全体实例同时"当选"双主）。
func (e *Elector) acquire(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := e.locker.TryLock(ctx)
		if err == nil && ok {
			return nil
		}
		if err := sleepWithContext(ctx, nextRetry(e.retryPeriod)); err != nil {
			return err
		}
	}
}

// renew 领导期主循环（与 Run 同一 goroutine），返回让位原因：
// nil 表示因 startedDone（业务返回）或 ctx.Done 之外的丢失路径退出
// 时携带 ErrLeadershipLost 系错误；优雅退出返回 ctx.Err()。
//
// deadlineTimer 实现 RenewDeadline 预算：距上一次续约成功超过预算即
// 在租约到期前主动让位（宁可误让位不可双主）；每次成功续约后重置。
// renewTimer 每轮重新采样抖动（禁用固定 Ticker，无法逐轮 jitter）。
func (e *Elector) renew(ctx context.Context, startedDone <-chan struct{}) error {
	var lastErr error // 最后一次续约错误（预算耗尽时附入 ErrLeadershipLost）

	deadline := time.Now().Add(e.renewDeadline) // 上次续约成功时刻 + 预算
	renewTimer := time.NewTimer(nextRetry(e.retryPeriod))
	defer renewTimer.Stop()
	deadlineTimer := time.NewTimer(e.renewDeadline)
	defer deadlineTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err() // 优雅退出
		case <-deadlineTimer.C:
			return errLost(lastErr) // 续约预算耗尽
		case <-startedDone:
			return nil // 业务主动让位（resigned）
		case <-renewTimer.C:
			budget := time.Until(deadline)
			if budget <= 0 {
				// S1：预算已尽，不再发注定超时的续约，直接让位。
				// 保守侧：宁可提前一步让位，不给后端添无意义请求。
				return errLost(lastErr)
			}
			renewCtx, cancel := context.WithTimeout(ctx, budget)
			ok, err := e.locker.Renew(renewCtx, e.leaseDuration)
			cancel()
			switch {
			case err == nil && ok:
				deadline = time.Now().Add(e.renewDeadline)
				deadlineTimer.Reset(e.renewDeadline)
				lastErr = nil
			case err != nil && errors.Is(err, lock.ErrRenewUnsupported):
				// 致命配置错误：立即让位并透出原因，errors.Is 同时
				// 命中 ErrLeadershipLost 与 lock.ErrRenewUnsupported。
				return errors.Join(ErrLeadershipLost, err)
			case err == nil && !ok:
				// (false, nil) 是确定性丢失信号：立即让位，不等预算
				// 耗尽（双主窗口压缩到 ≤ 1.25×RetryPeriod + 一次调用耗时）。
				return errLost(nil)
			default:
				// err != nil 一律视为失败（含 FailOpen 的 (true, err)
				// 组合），进入预算倒计时。
				lastErr = err
			}
			renewTimer.Reset(nextRetry(e.retryPeriod))
		}
	}
}

// stepDown 统一让位清理（五种让位路径共用：优雅退出/确认丢失/预算
// 耗尽/致命配置错/主动让位；"当选瞬间取消"不经过本函数——它从未
// started，清理序列不同）。顺序不可变：
// isLeader 置 false → cancelLead（业务 ctx 取消）→ 尽力而为 Unlock →
// OnStoppedLeading（恰好一次，若已设置）。
//
// cancelLead 之后不等待 OnStartedLeading 返回即释放锁（与 client-go
// 的 defer 顺序一致）：业务对取消的响应时延构成理论双写窗口，以 term
// 贯通缓解，见 package doc。
func (e *Elector) stepDown(cancelLead context.CancelFunc) {
	e.isLeader.Store(false)
	cancelLead()
	e.releaseLock()
	if e.callbacks.OnStoppedLeading != nil {
		e.callbacks.OnStoppedLeading()
	}
}

// releaseLock 尽力而为释放锁：原 ctx 此时通常已取消，直接传入会让
// Release 立即失败、锁残留至 TTL，故以 context.Background 为父脱离
// 取消传播——不继承原 ctx values（Unlock 不需要），独立超时
// releaseTimeout 防后端挂起时无限阻塞。结果不检查——失败
// 由 TTL 兜底（自最后一次成功续约起 ≤ LeaseDuration 自然过期）。
func (e *Elector) releaseLock() {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	_ = e.locker.Unlock(ctx)
}

// errLost 构造 leadership lost 错误。cause 为 nil 时对应 (false, nil)
// 的确定性丢失信号（无具体错误对象），使用固定文案；非 nil 时以
// "%w: %v" 附加哨兵与原因，errors.Is(结果, ErrLeadershipLost) 命中。
func errLost(cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: renew confirmed lock lost", ErrLeadershipLost)
	}
	return fmt.Errorf("%w: %v", ErrLeadershipLost, cause)
}

// nextRetry 返回带抖动的重试间隔：d + rand[0, d/4)，即 [1.0, 1.25)×d
// （对齐 client-go JitterFactor=1.2 的量级）。d/4 整除为 0（d < 4ns）
// 时返回 d，避免零间隔忙轮询。随机源 math/rand（非加密用途，与
// retry 包 backoff.go 一致）。
func nextRetry(d time.Duration) time.Duration {
	if jitter := d / 4; jitter > 0 {
		return d + time.Duration(mrand.Int63n(int64(jitter)))
	}
	return d
}

// defaultIdentity 返回 "hostname-pid"；hostname 获取失败时用 "unknown"。
func defaultIdentity() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return host + "-" + strconv.Itoa(os.Getpid())
}

// sleepWithContext 等待 d 后返回 nil；期间 ctx 取消/超时则返回
// ctx.Err()。d <= 0 不睡，仅检查 ctx。使用 time.Timer + select，
// 确保可被 ctx 中断（对齐 retry 包 defaultSleep 模式）。
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
