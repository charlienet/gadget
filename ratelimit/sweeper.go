package ratelimit

import (
	"time"
)

// sweepLoop 是本地账本闲置回收循环（仅租约模式启动）：
// time.Timer 周期 = IdleRetention/2，select 仅 stopChan——Limiter 不持
// 外部 ctx，受控退出走 Close（Once + stopped + WaitGroup，对齐 cache
// 先例，不加 recover）。
func (l *Limiter) sweepLoop() {
	defer l.wg.Done()

	interval := l.spec.IdleRetention / 2
	if interval <= 0 {
		interval = time.Second
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			l.sweepOnce(l.clock.Now())
			timer.Reset(interval)
		case <-l.stopped:
			return
		}
	}
}

// sweepOnce 删除闲置超阈的账本条目。
//
// 锁纪律：外层持 ledger.mu 遍历、逐条目持有 ent.mu 复检后才删——
// 防止"正被持有的瞬间删除"（宁可延后一轮）；在途批发（pending 非 nil）
// 的条目跳过，批发结算不被悬空条目吞掉。
//
// 被删条目的未用存量一并丢弃：这些令牌是已向 Backend 批发的租约，
// 浪费上界受批量约束且靠后端桶自然回补（v1 不做 giveback）。
func (l *Limiter) sweepOnce(now time.Time) {
	ld := l.ledger

	ld.mu.Lock()
	defer ld.mu.Unlock()

	for key, ent := range ld.entries {
		ent.mu.Lock()
		idle := ent.pending == nil && now.Sub(ent.idleAt) > l.spec.IdleRetention
		ent.mu.Unlock()
		if idle {
			delete(ld.entries, key)
		}
	}
}
