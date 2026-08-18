package cache

import (
	"sync"
	"sync/atomic"
)

type storeStats struct {
	Hits uint64
	Miss uint64
}

func (s storeStats) Total() uint64 {
	return s.Hits + s.Miss
}

type Stats struct {
	stores    map[string]*storeStats
	Query     uint64
	QueryFail uint64
	Shared    uint64
	// l 为指针锁：Stats 可作为值拷贝返回（Snapshot 只读快照），
	// 指针锁的拷贝不触发 copylocks 检查；内部方法经 s.l 自动解引用。
	l *sync.Mutex
}

func newStats() Stats {
	return Stats{stores: make(map[string]*storeStats), l: &sync.Mutex{}}
}

func (s *Stats) IncrHit(name string) {
	s.l.Lock()
	defer s.l.Unlock()

	if v, ok := s.stores[name]; ok {
		atomic.AddUint64(&v.Hits, 1)
	} else {

		s.stores[name] = &storeStats{Hits: 1}
	}
}

func (s *Stats) IncrMiss(name string) {
	s.l.Lock()
	defer s.l.Unlock()

	if v, ok := s.stores[name]; ok {
		atomic.AddUint64(&v.Miss, 1)
	} else {
		s.stores[name] = &storeStats{Miss: 1}
	}
}

func (s *Stats) IncrShared() {
	atomic.AddUint64(&s.Shared, 1)
}

func (s *Stats) IncrQuery() {
	atomic.AddUint64(&s.Query, 1)
}

func (s *Stats) IncrQueryFail(err error) {
	atomic.AddUint64(&s.QueryFail, 1)
}

func (s *Stats) TotalHits() uint64 {
	s.l.Lock()
	defer s.l.Unlock()

	var total uint64
	for _, v := range s.stores {
		total += v.Hits
	}

	return total
}

func (s *Stats) TotalMiss() uint64 {
	s.l.Lock()
	defer s.l.Unlock()

	var total uint64
	for _, v := range s.stores {
		total += v.Miss
	}

	return total
}

func (s *Stats) Total() uint64 {
	s.l.Lock()
	defer s.l.Unlock()

	var total uint64
	for _, v := range s.stores {
		total += v.Total()
	}

	return total
}

// Snapshot 返回一致性只读快照（值拷贝）：外部修改快照不影响内部计数。
// Query/QueryFail/Shared 用 atomic 读取；stores 在锁内深拷贝（每个 storeStats
// 值复制）。返回后快照与内部完全隔离。
func (s *Stats) Snapshot() Stats {
	s.l.Lock()
	defer s.l.Unlock()

	snap := Stats{
		stores:    make(map[string]*storeStats, len(s.stores)),
		Query:     atomic.LoadUint64(&s.Query),
		QueryFail: atomic.LoadUint64(&s.QueryFail),
		Shared:    atomic.LoadUint64(&s.Shared),
		l:         &sync.Mutex{},
	}
	for name, v := range s.stores {
		snap.stores[name] = &storeStats{
			Hits: atomic.LoadUint64(&v.Hits),
			Miss: atomic.LoadUint64(&v.Miss),
		}
	}
	return snap
}

func (s *Stats) Clear() {
	s.l.Lock()
	defer s.l.Unlock()

	for _, v := range s.stores {
		atomic.SwapUint64(&v.Hits, 0)
		atomic.SwapUint64(&v.Miss, 0)
	}

	atomic.SwapUint64(&s.Query, 0)
	atomic.SwapUint64(&s.QueryFail, 0)
	atomic.SwapUint64(&s.Shared, 0)
}
