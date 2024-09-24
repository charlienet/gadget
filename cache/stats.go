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
	l         sync.Mutex
}

func newStats() Stats {
	return Stats{stores: make(map[string]*storeStats)}
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

func (s *Stats) Clear() {
	s.l.Lock()
	defer s.l.Unlock()

	for _, v := range s.stores {
		atomic.SwapUint64(&v.Hits, 0)
		atomic.SwapUint64(&v.Miss, 0)
	}

	query := atomic.SwapUint64(&s.Query, 0)
	queryFail := atomic.SwapUint64(&s.QueryFail, 0)

	_ = query
	_ = queryFail
}
