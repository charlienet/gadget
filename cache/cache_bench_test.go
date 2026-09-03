package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// 本基准文件仅使用改造前后两版 mem_store 共有的 API（newMemStore / Put / Get /
// maxItems / ttlJitter 字段），因此可在旧实现（RLock 两阶段 + insertOrder FIFO）
// 与新实现（侵入式链表 LRU + 全锁单阶段）上分别运行，用于对比吞吐。

// BenchmarkConcurrentGet 度量命中密集场景下的读吞吐，按 1/8/64 goroutine 展开。
// 改造前 Get 用 RLock（多读并发），改造后为配合 LRU 提升改用全互斥 Lock——
// 该对比直观呈现锁策略变更对读并发的影响（数据如实报告，不据此擅改锁策略）。
func BenchmarkConcurrentGet(b *testing.B) {
	for _, g := range []int{1, 8, 64} {
		g := g
		b.Run(fmt.Sprintf("goroutines=%d", g), func(b *testing.B) {
			benchConcurrentGet(b, g)
		})
	}
}

func benchConcurrentGet(b *testing.B, goroutines int) {
	s := newMemStore()
	s.ttlJitter = 0
	ctx := context.Background()

	const keys = 2000
	// 预生成 key 切片：把 fmt.Sprintf 移出热循环。否则每次 Get 都新建一个 key 字符串
	// （实测约 14 B/op 分配即源于此），污染读吞吐测量。ResetTimer 前完成，不计入基准。
	keyList := make([]string, keys)
	for i := 0; i < keys; i++ {
		keyList[i] = fmt.Sprintf("k%d", i)
		_ = s.Put(ctx, keyList[i], []byte("payload-bytes"), 0) // expire 0：不过期，保证命中
	}

	per := b.N / goroutines
	if per < 1 {
		per = 1
	}

	b.ResetTimer()
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				_, _, _ = s.Get(ctx, keyList[(g*131+i)%keys]) // 直接取预生成 key：热循环零分配
			}
		}(g)
	}
	wg.Wait()
	b.StopTimer()
}

// BenchmarkSerialPutEviction 度量单线程持续 Put 触发容量驱逐时的写入吞吐。
// 注意：本基准（全新建键 + 满容量驱逐）的成本主要由 evictIfNeeded 第一阶段的全表
// 过期扫描支配——该扫描两版共有、且按设计保持 O(n) 不做优化，故新旧版数字接近，
// 本基准并非最能体现数据结构改进的场景。侵入式链表把 removeFromOrder 的 O(n) 线性
// 摘除降为 O(1)，其真实收益体现在大表"覆写已有 key"与"Delete"路径，而非此处全新建键。
func BenchmarkSerialPutEviction(b *testing.B) {
	s := newMemStore()
	s.ttlJitter = 0
	s.maxItems = 1000
	ctx := context.Background()

	for i := range 1000 {
		_ = s.Put(ctx, fmt.Sprintf("warm%d", i), []byte("v"), 0)
	}

	for i := 0; b.Loop(); i++ {
		_ = s.Put(ctx, fmt.Sprintf("k%d", i), []byte("v"), 0)
	}
	b.StopTimer()
}
