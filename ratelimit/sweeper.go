package ratelimit

import (
	"time"
)

// sweepLoop 是闲置回收循环（租约模式，或后端为 memoryBackend 时启动）：
// time.Timer 周期 = IdleRetention/2，select 仅 stopChan——Limiter 不持
// 外部 ctx，受控退出走 Close（Once + stopped + WaitGroup，对齐 cache
// 先例，不加 recover）。同一 tick 顺带驱动 memoryBackend.buckets 的条目
// 回收（sweepOnce 内 reapIdle），故后端无需自持协程，防 goroutine 泄漏。
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

// sweepOnce 删除闲置超阈的账本条目，并驱动后端条目回收。
//
// 锁纪律：外层持 ledger.mu 遍历、逐条目持有 ent.mu 复检后才删——
// 防止"正被持有的瞬间删除"（宁可延后一轮）；在途批发（pending 非 nil）
// 的条目跳过，批发结算不被悬空条目吞掉。
//
// TOCTOU 修复：delete 移入 ent.mu 持有区间内（复检与删除原子），不再
// "释放 ent.mu 后才 delete"——后者存在窗口：已持有 ent 引用的 goroutine
// 可在释放瞬间重新置 pending/idleAt，令本轮删除悬空化。
//
// 被删条目的未用存量一并丢弃：这些令牌是已向 Backend 批发的租约，
// 浪费上界受批量约束且靠后端桶自然回补（v1 不做 giveback）。
func (l *Limiter) sweepOnce(now time.Time) {
	ld := l.ledger

	ld.mu.Lock()
	for key, ent := range ld.entries {
		ent.mu.Lock()
		idle := ent.pending == nil && now.Sub(ent.idleAt) > l.spec.IdleRetention
		if idle {
			delete(ld.entries, key)
		}
		ent.mu.Unlock()
	}
	ld.mu.Unlock()

	// 复用本 tick 回收 memoryBackend.buckets 条目（无界 key 空间的内存
	// 增长防线）。reapIdle 内部自持 mb.mu；本函数在释放 ld.mu 之后才
	// 调用它——顺序取锁，从不嵌套持有两把锁，无环、不死锁。
	if mb, ok := l.backend.(*memoryBackend); ok {
		mb.reapIdle(now, l.spec.IdleRetention)
	}
}
