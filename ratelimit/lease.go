package ratelimit

import (
	"context"
	"errors"
	"sync"
	"time"
)

// entry 是单个 key 的本地租约账本（纯存量）。
//
// 不变式（H1 防线）：remain 只能被批发 granted 注入、被 Allow 扣减，
// 任何路径都不得按速率自补充——速率语义 100% 归 Backend。
type entry struct {
	mu sync.Mutex // 临界区内只做：判存量 / 判静默期 / 登记与消费 pending，绝不网络等待

	remain       int           // 本地存量（只能被注入/扣减）
	silenceUntil time.Time     // 静默期终点：批发返回 0 后 retryAfter 窗口内不再打远端
	idleAt       time.Time     // 最近一次访问时间（sweeper 闲置回收判据）
	pending      *pendingBatch // 在途批发（非 nil 表示有 leader 正在批发）
}

// pendingBatch 是一次在途批发：leader 执行并填写 res 后 close(done) 广播，
// followers 在锁外等待并共享同一裁决结果。
type pendingBatch struct {
	done chan struct{} // res 写入后关闭（happens-before 保证并发读安全）
	res  batchResult
}

// batchResult 是一次批发的原始结果（分诊在消费端按同一策略执行）。
type batchResult struct {
	granted    int
	retryAfter time.Duration
	err        error
}

// ledger 管理 per-key 账本条目；外层锁只保护 map 结构，条目状态归各自 mu。
type ledger struct {
	mu      sync.Mutex
	entries map[string]*entry
}

func newLedger() *ledger {
	return &ledger{entries: make(map[string]*entry)}
}

// getOrCreate 返回 key 的账本条目（不存在则创建并记 idleAt）。
func (ld *ledger) getOrCreate(key string, now time.Time) *entry {
	ld.mu.Lock()
	defer ld.mu.Unlock()

	if e, ok := ld.entries[key]; ok {
		return e
	}
	e := &entry{idleAt: now}
	ld.entries[key] = e
	return e
}

// leaseAllow 租约模式主流程：热路径内存扣减 →（存量不足）in-flight 合并
// 批发 → 重新持锁扣减一次。单次调用至多一次批发，不循环打远端。
func (l *Limiter) leaseAllow(ctx context.Context, key string, n int) (bool, error) {
	now := l.clock.Now()
	ent := l.ledger.getOrCreate(key, now)

	// 阶段 1：热路径判定（锁内零网络）。
	ent.mu.Lock()
	ent.idleAt = now
	if ent.remain >= n {
		ent.remain -= n
		ent.mu.Unlock()
		return true, nil
	}
	if now.Before(ent.silenceUntil) {
		retry := ent.silenceUntil.Sub(now)
		ent.mu.Unlock()
		return false, &ExceededError{Key: key, N: n, RetryAfter: retry}
	}

	// 阶段 2：批发合并登记——已有在途批发则做 follower 共享其结果，
	// 否则登记自己为 leader，锁外发起批发。
	var p *pendingBatch
	leader := ent.pending == nil
	if leader {
		p = &pendingBatch{done: make(chan struct{})}
		ent.pending = p
	} else {
		p = ent.pending
	}
	ent.mu.Unlock()

	if !leader {
		// follower：锁外等待共享结果，可被自己的 ctx 唤醒（不殃及他人）。
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-p.done:
		}
		ent.mu.Lock()
		defer ent.mu.Unlock()
		return l.settle(ctx, ent, p.res, key, n)
	}

	// leader：锁外批发并结算广播（独立方法，defer 兜底 Backend panic）。
	return l.leaderBatch(ent, p, ctx, key, n)
}

// leaderBatch 执行 leader 侧批发：内部 ctx 打后端 → 持锁结算 → 广播 →
// 参与一次扣减。
//
// Backend panic 防护（N-A）：若第三方 Backend 在 Wholesale 中 panic，
// defer 兜底保证当次批发一定被"终止"——清理 pending、以内部错误
// errBrokenBatch 结算并 close(done)，followers 从共享等待中收到错误而非
// 永久挂死。**panic 本身继续穿透**（本包不做 recover，与 breaker/retry
// 的"不吞 panic"哲学一致；等待者已解除挂起后，panic 沿 leader 栈上抛
// 给它的调用方）。
func (l *Limiter) leaderBatch(ent *entry, p *pendingBatch, ctx context.Context, key string, n int) (bool, error) {
	broadcast := false
	defer func() {
		if broadcast {
			return
		}
		// panic 路径兜底：当次批发未正常结算即中断。
		ent.mu.Lock()
		defer ent.mu.Unlock() // panic 后 ent.mu 绝不可滞留持有，否则该 key 永久瘫痪
		if ent.pending == p {
			ent.pending = nil
		}
		if p.res.err == nil {
			p.res = batchResult{err: errBrokenBatch}
		}
		close(p.done)
	}()

	// 内部 ctx 批发（context.WithTimeout(context.Background(), …)），
	// 不随单个请求 ctx 取消而殃及共享者。
	p.res = l.wholesale(key)

	// N-F：同一批发结果的 FailOpen 兜底日志只由 leader 记一条，且在
	// ent.mu 临界区之外——followers 共享同一错误返回（ErrFailOpen 可判，
	// 处置归应用层），不重复刷日志（防故障期日志风暴）。
	l.logFailOpenFallback(ctx, p.res.err, key, n)

	ent.mu.Lock()
	defer ent.mu.Unlock()

	// 结算（仍持 ent.mu）：清 pending、注入存量或设置静默期、广播。
	granted, retryAfter, werr := p.res.granted, p.res.retryAfter, p.res.err
	if werr == nil {
		if granted > 0 {
			ent.remain += granted
		} else {
			if retryAfter < 0 {
				retryAfter = 0
			}
			ent.silenceUntil = l.clock.Now().Add(retryAfter)
		}
	}
	ent.pending = nil
	broadcast = true // close 前先行标记：broadcast 之后不再有可 panic 的前置状态
	close(p.done)

	return l.settle(ctx, ent, p.res, key, n)
}

// settle 在持有 ent.mu 时对批发原始结果做分诊与一次扣减
// （leader 与 followers 共享同一裁决）。
func (l *Limiter) settle(ctx context.Context, ent *entry, res batchResult, key string, n int) (bool, error) {
	ent.idleAt = l.clock.Now()

	if res.err != nil {
		return l.triageBackendErr(ctx, res.err, key, n)
	}
	// 批发已成功记账；等待者自身 ctx 已取消则不占用存量（透传，不进分诊）。
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if ent.remain >= n {
		ent.remain -= n
		return true, nil
	}
	// 注入后仍不足（多等待者分摊）：直接超限返回，不再循环打远端。
	retry := res.retryAfter
	if retry <= 0 {
		retry = l.nominalDelay(n)
	}
	return false, &ExceededError{Key: key, N: n, RetryAfter: retry}
}

// wholesale 以内部 ctx 发起一次批量租约（BestEffort）。
func (l *Limiter) wholesale(key string) batchResult {
	ctx, cancel := context.WithTimeout(context.Background(), l.backendTimeout)
	defer cancel()

	granted, retryAfter, err := l.backend.Wholesale(ctx, key, l.want(), l.spec, GrantBestEffort)
	if err != nil {
		return batchResult{err: err}
	}
	return batchResult{granted: granted, retryAfter: retryAfter}
}

// triageBackendErr 按 §6 错误契约分诊后端错误（持有 ent.mu 时被调用，
// 实现者不得在此等待）：
//   - 含 ErrBackendUnavailable → 按 FailPolicy：Open 全员放行并返回
//     双可判包装错误；Closed 全员拒绝返回包装错误；
//   - 其余错误（命令级、内部批发 ctx 超时等）原样透传、不进兜底。
//
// 本函数**不记日志**（N-F）：兜底事件日志由批发发起方（leaderBatch /
// strictAllow）在锁外统一记录——同一批发结果恰一条，followers 共享裁决
// 时不重复输出（防故障期日志风暴）；错误返回值与双可判语义不变。
func (l *Limiter) triageBackendErr(ctx context.Context, err error, key string, n int) (bool, error) {
	if errors.Is(err, ErrBackendUnavailable) {
		if l.policy == FailOpen {
			return true, wrapFailOpen(err)
		}
		return false, wrapUnavailable(err)
	}
	return false, err
}

// logFailOpenFallback 在后端不可用错误即将按 FailOpen 兜底放行时记录一条
// Warn。调用约束：批发发起方（leaderBatch 结算前 / strictAllow 独立批发）、
// ent.mu 临界区之外——同一批发结果恰记录一次。
func (l *Limiter) logFailOpenFallback(ctx context.Context, err error, key string, n int) {
	if err != nil && errors.Is(err, ErrBackendUnavailable) && l.policy == FailOpen {
		l.logger.WarnContext(ctx, "后端不可用，按 FailOpen 兜底放行",
			"key", key, "n", n, "err", err)
	}
}
