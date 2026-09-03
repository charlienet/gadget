// Package lifecycle 提供进程退出时的优雅关闭编排能力。
//
// # 顺序约定
//
// 组件的注册顺序即依赖顺序：被依赖者先注册，依赖者后注册；进程入口
// （最外层、最后退出的东西，例如 HTTP 服务器）必须最后注册。关闭时严格按
// 注册的逆序、逐个串行执行，因此入口最先关闭、最底层依赖最后关闭。
//
// 注意：本包不实现任何依赖图或拓扑排序，逆序完全由注册顺序决定。若某个组件
// 依赖另一组件仍存活，二者注册先后关系必须正确，否则关闭顺序不受保护。
//
// # 级联关闭：组件级事实，不可外推
//
// 关闭是逐个组件独立调用 Stop 的过程，一个组件失败（返回 error、超时或
// panic）不会中断其余组件的关闭流程，错误仅被收集并最终聚合返回。调用方不应
// 假设某个组件关闭失败会“连锁”跳过后续组件，反之也不应假设后续组件能感知到
// 前序组件已经失败——组件之间彼此不可见。
//
// 至于“某个组件关闭时会不会顺手把它内部依赖也关掉”，那是该组件自身的实现
// 事实，只能逐个查证，不能当作通则。本仓库当前的实情是：
//
//   - cache 的 Close 确实会级联关闭其内部的 localStore / remoteStore
//     （见 cache 包 Cache.Close 中对二者的 closer 类型断言处）。
//   - redis 的 GracefulClose 确实会级联关闭由 AddPrefix 派生出的全部子连接池
//     （见 redis 包 GracefulClose 的子池级联循环处）。
//   - 但上述级联仅对实现了 `interface{ Close() }`（无返回值）的内部依赖生效；
//     例如 redis 包客户端的签名是 `Close() error`，二者不兼容，类型断言
//     失败后该依赖会被静默跳过——也就是说 cache 并不会替你关掉一个 redis 客户端。
//
// 结论：需要被关闭的底层依赖（redis 客户端等）一律独立 Register，不要
// 指望容器组件代为关闭；万一出现重复关闭，由 [Component] 的幂等契约兜底。
//
// # 触发路径
//
// 关闭可由两条路径启动，二者语义完全一致，可以混用：
//
//   - 运行期事件驱动：调用 [Manager.Run]，它会监听 OS 信号与传入 ctx 的取消，
//     任一发生即启动关闭。
//   - 显式编程驱动：调用 [Manager.Shutdown]，立即启动关闭。
//
// 无论由哪条路径、哪种触发源启动，关闭只执行一次；其余后到的 Run/Shutdown
// 调用（含并发）会阻塞等待同一次关闭完成，并返回同一份聚合错误。
//
// # 双路径关闭
//
// 若一个容器组件（例如 cache）与其内部依赖（例如它所用的 store）同时被注册，
// 关闭时就会形成两条到达同一底层资源的路径：容器组件的关闭逻辑关一次，独立
// 注册的依赖自己再关一次。本包既不检测也不去重这种重叠，但 [Component] 的幂等
// 契约保证第二次调用不产生额外副作用，因此实害为零，只是多了一次调用。
//
// # logger 自指顺序
//
// 若把日志器本身作为一个组件注册（例如异步 logger 需要在退出时 flush），它必须
// 最先注册（因而在整个关闭流程的最后一步才被关闭），以保证其它组件关闭期间仍
// 有可用的日志输出。反过来，若 logger 最后注册则它最先关闭，其它组件的关闭日志
// 将会丢失。
//
// # 二次信号是 OS 默认强杀（有意为之）
//
// 关闭一经启动，本包会立即通过 signal.Stop 注销信号句柄。因此关闭过程中若再收到
// 同种信号，将不再被本包捕获，而是交由操作系统执行默认动作（终止进程）。这是刻意
// 设计：为“优雅关闭卡住时用户强杀进程”保留逃生通道，而非缺陷。
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"time"
)

// Component 是参与优雅关闭的基本单元。
//
// 实现必须满足以下契约：
//
//  1. 幂等：重复调用 Stop 不产生额外副作用，也不改变已停止的状态。
//  2. 响应 ctx：Stop 应尊重传入 ctx 的取消/超时，在 ctx.Done 时尽快返回，
//     不得无视 ctx 而无限阻塞（否则该步会被记为 [ErrTimeout] 并跳过）。
//  3. 彻底退出：Stop 返回时，该组件内部启动的所有 goroutine 必须已全部退出，
//     不得遗留后台任务。
type Component interface {
	Stop(ctx context.Context) error
}

// Func 把普通函数适配为 [Component]，方便注册无需自定义类型的关闭逻辑。
//
// 方法值可直接桥接不同签名的关闭 API，例如 http.Server 的
// Shutdown(ctx) error 可直接以 Func(srv.Shutdown) 注册；返回 error 的
// Close() 则包一层闭包 Func(func(ctx context.Context) error { return c.Close() })。
type Func func(ctx context.Context) error

// Stop 调用 f 本身，使 Func 满足 [Component]。
func (f Func) Stop(ctx context.Context) error { return f(ctx) }

// Manager 编排多个 [Component] 的优雅关闭。零值不可用，请使用 [New] 构造。
type Manager struct {
	opts options

	mu         sync.Mutex
	components []entry
	state      state
	sigCh      chan os.Signal // 首个 Run 注册的信号句柄；非 nil 即表示已在监听

	once   sync.Once
	done   chan struct{} // 关闭流程完全结束时被 close，用于广播
	aggErr error         // 聚合错误；先写入本字段，后 close(done) 建立可见性
}

type entry struct {
	name string
	comp Component
}

type state int

const (
	stateIdle state = iota
	stateRunning
)

// New 构造一个 Manager，并在返回前统一校验所有 [Option]。
// 非法选项（见各 Option 文档）会直接 panic，因为这属于程序期错误。
func New(opts ...Option) *Manager {
	o := options{
		stepTimeout:  defaultStepTimeout,
		totalTimeout: 0, // 0 = 未设置，不设总时限
		signals:      append([]os.Signal(nil), defaultSignals...),
		logger:       nil,
	}
	for _, fn := range opts {
		fn(&o)
	}
	return &Manager{
		opts: o,
		done: make(chan struct{}),
	}
}

// Register 以给定名称注册一个组件。注册顺序即依赖顺序，进程入口应最后注册。
//
// Register 用互斥锁保护内部组件表与状态，可被多个 goroutine 并发调用。
//
// Register 在以下任一情况 panic：
//
//   - name 为空字符串；
//   - name 与已注册组件重复；
//   - c 为 nil；
//   - 关闭已经触发（Run 的 ctx/信号触发，或已调用过 Shutdown）之后再注册。
func (m *Manager) Register(name string, c Component) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state != stateIdle {
		panic("lifecycle: Register called after shutdown was triggered")
	}
	if name == "" {
		panic("lifecycle: Register called with empty name")
	}
	if c == nil {
		panic(fmt.Sprintf("lifecycle: Register %q with nil component", name))
	}
	for _, e := range m.components {
		if e.name == name {
			panic(fmt.Sprintf("lifecycle: duplicate component name %q", name))
		}
	}
	m.components = append(m.components, entry{name: name, comp: c})
}

// Components 返回当前已注册组件名称的快照，顺序即注册顺序。
// 返回的是内部切片的副本，调用方对其修改不会影响 Manager。
func (m *Manager) Components() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, len(m.components))
	for i, e := range m.components {
		names[i] = e.name
	}
	return names
}

// Run 阻塞监听关闭触发源，并在关闭完成后返回聚合错误。
//
// 调用 Run 会注册 [Option.WithSignals] 指定的 OS 信号并监听传入 ctx 的取消；
// 信号到达、ctx 取消，或另一 goroutine 调用 [Manager.Shutdown]，任一发生都会
// 启动关闭。若关闭已由其它路径启动，本调用不会重复注册信号或重复触发，而是直接
// 阻塞等待同一次关闭完成。
//
// 多个 Run 并发时，只有首个 Run 创建的信号句柄与监听的 ctx 生效；后续 Run 不再
// 注册句柄，也不会监听自己的 ctx——它的 ctx 被取消不会触发任何东西，只会继续等待
// 同一次关闭完成。需要额外触发源请显式调用 [Manager.Shutdown]。
//
// 返回路径与触发方式无关：正常而言 Run 总是等待关闭流程全部执行完毕后，返回关闭
// 的聚合错误（全部成功时为 nil）。Run 不因 ctx 取消而提前返回取消错误——ctx 取消
// 只是关闭的“原因”，关闭流程仍会完整走完。
func (m *Manager) Run(ctx context.Context) error {
	// 仅在尚未触发关闭、且监听未启动时，注册信号并拉起监听 goroutine。
	// 判据是 m.sigCh 是否已存在：它的赋值与 signal.Notify 在同一 m.mu 临界区内
	// 完成，因此 sigCh != nil 严格等价于“句柄已注册并在监听”。与 start() 对
	// state/sigCh 的读写都在 m.mu 下，保证并发安全且线性化。
	var sigCh chan os.Signal
	startWatch := false
	m.mu.Lock()
	if m.state == stateIdle && m.sigCh == nil {
		sigCh = make(chan os.Signal, 1) // 缓冲 >=1，避免 signal.Notify 丢弃首信号
		signal.Notify(sigCh, m.opts.signals...)
		m.sigCh = sigCh // 回写字段，使 start() 的 signal.Stop 真实生效
		startWatch = true
	}
	m.mu.Unlock()

	if startWatch {
		go func() {
			select {
			case sig := <-sigCh:
				m.logf("received signal %v, starting shutdown", sig)
				m.start()
			case <-ctx.Done():
				m.logf("run context done (%v), starting shutdown", ctx.Err())
				m.start()
			}
		}()
	}

	// 等待关闭完成，返回聚合错误。
	return m.await()
}

// Shutdown 启动关闭并返回聚合错误。
//
// 若关闭尚未启动，本调用会触发它；若已启动（无论来自信号、ctx 还是另一次
// Shutdown），本调用不会重复触发。关闭流程始终会完整执行，与 ctx 无关。
//
// 传入的 ctx 仅控制“是否等待关闭完成”：若在关闭完成前 ctx 被取消，Shutdown
// 立即返回 ctx.Err()，但关闭本身继续在后台推进，其它等待者仍会收到完整的聚合错误。
// 若关闭在 ctx 取消前已完成，则返回关闭的聚合错误（全部成功时为 nil）。
func (m *Manager) Shutdown(ctx context.Context) error {
	m.start()
	select {
	case <-m.done:
		return m.aggErr
	case <-ctx.Done():
		// ctx 与关闭完成同时就绪时优先返回聚合错误，兑现“关闭已完成则返回聚合错误”。
		select {
		case <-m.done:
			return m.aggErr
		default:
		}
		return ctx.Err()
	}
}

// start 幂等地启动一次关闭流程，启动后立即返回、不等待完成：由 sync.Once 保证
// 只启动一次。启动动作为置状态、对组件表取快照、注销信号句柄，并在独立 goroutine
// 中执行 shutdownAll；完成时先写 aggErr，再 close(done) 向所有等待者广播。
//
// 之所以把 shutdownAll 放到 goroutine 里，是为了让 [Manager.Shutdown] 的 ctx 在
// “首个触发源就是 Shutdown”时同样有效：start 不阻塞，Shutdown 才能对 done 与
// ctx.Done 做 select，而关闭流程照常推进。
func (m *Manager) start() {
	m.once.Do(func() {
		// 进入 Running，并在同一把锁内对组件表取快照，与并发 Register 线性化：
		// 锁之前完成的注册进入本次快照，锁之后的注册会被 Register panic。
		m.mu.Lock()
		m.state = stateRunning
		comps := make([]entry, len(m.components))
		copy(comps, m.components)
		sigCh := m.sigCh
		m.mu.Unlock()

		// 关闭启动即注销信号句柄：此后同种信号交回 OS 默认处理（强杀）。
		// 这里同步执行、且在拉起关闭 goroutine 之前，因此“组件 Stop 被调用”
		// 必然发生在本行之后，测试可据此做确定性同步。
		if sigCh != nil {
			signal.Stop(sigCh)
		}

		go func() {
			// 关闭根 ctx 与触发源无关：cancel 是关闭的原因，不是关闭的中断。
			rootCtx := context.Background()
			if m.opts.totalTimeout > 0 {
				var cancel context.CancelFunc
				rootCtx, cancel = context.WithTimeout(rootCtx, m.opts.totalTimeout)
				defer cancel()
			}

			err := m.shutdownAll(rootCtx, comps)

			// 先写聚合错误，后 close(done)，靠 happens-before 保证可见性。
			m.aggErr = err
			close(m.done)
		}()
	})
}

// await 阻塞等待关闭流程完成，返回其聚合错误（全部成功时为 nil）。
func (m *Manager) await() error {
	<-m.done
	return m.aggErr
}

// shutdownAll 按注册逆序、串行地关闭快照中的组件，返回聚合错误。
// 一旦根 ctx（总预算）耗尽，尚未关闭的剩余组件全部跳过并计入 [SkippedError]。
func (m *Manager) shutdownAll(rootCtx context.Context, comps []entry) error {
	var errs []error
	for i := len(comps) - 1; i >= 0; i-- {
		// 每步开始前检查总预算：若已耗尽，comps[0..i] 均未被调用，全部跳过。
		if rerr := rootCtx.Err(); rerr != nil {
			skipped := make([]string, 0, i+1)
			for _, e := range comps[:i+1] {
				skipped = append(skipped, e.name)
			}
			// 只放 *SkippedError：它 Unwrap 到 ErrBudgetExhausted，
			// errors.Is 判定成立，无需再裸 append 一个哨兵造成文案重复。
			errs = append(errs, &SkippedError{Names: skipped})
			return errors.Join(errs...)
		}

		if serr := m.runStep(rootCtx, comps[i]); serr != nil {
			errs = append(errs, serr)
		}
	}
	return errors.Join(errs...)
}

// runStep 在独立 goroutine 中执行单个组件的 Stop，用 stepCtx 施加超时。
// 超时后残留 goroutine 不会被 kill（Go 无法强杀 goroutine），流程继续下一步。
func (m *Manager) runStep(rootCtx context.Context, e entry) error {
	// stepCtx = 根 ctx 之上再限 min(剩余预算, stepTimeout)。
	timeout := m.opts.stepTimeout
	if dl, ok := rootCtx.Deadline(); ok {
		if remaining := time.Until(dl); remaining < timeout {
			timeout = remaining
		}
	}
	stepCtx, cancel := context.WithTimeout(rootCtx, timeout)
	defer cancel()

	m.logf("stopping component %q", e.name)
	start := time.Now()

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- m.callStop(stepCtx, e)
	}()

	select {
	case serr := <-stopDone:
		elapsed := time.Since(start)
		if serr != nil {
			wrapped := fmt.Errorf("lifecycle: %s: %w", e.name, serr)
			m.logf("component %q failed after %s: %v", e.name, elapsed, serr)
			return wrapped
		}
		m.logf("component %q stopped in %s", e.name, elapsed)
		return nil
	case <-stepCtx.Done():
		m.logf("component %q timed out after %s", e.name, time.Since(start))
		return fmt.Errorf("lifecycle: %s: %w", e.name, ErrTimeout)
	}
}

// callStop 调用组件 Stop 并以 recover 包裹 panic。
// panic 被转为 wrap 了 [ErrPanicked] 与 recover 值的 error，并记录堆栈。
func (m *Manager) callStop(ctx context.Context, e entry) (err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			if m.opts.logger != nil {
				m.opts.logger.Error("component panicked during stop",
					"name", e.name, "panic", fmt.Sprint(r), "stack", string(stack))
			}
			err = fmt.Errorf("%w: %v\n%s", ErrPanicked, r, stack)
		}
	}()
	return e.comp.Stop(ctx)
}

// logf 在配置了 logger 时输出 Info 级日志，否则为空操作。
func (m *Manager) logf(format string, args ...any) {
	if m.opts.logger == nil {
		return
	}
	m.opts.logger.Info(fmt.Sprintf(format, args...))
}
