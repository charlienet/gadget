package bloom

import (
	"context"
	"sync"

	"github.com/bits-and-blooms/bitset"
)

type mem_store struct {
	size uint64
	bits *bitset.BitSet
	lock sync.RWMutex
}

func newMemStore(size uint64) *mem_store {
	return &mem_store{
		size: size,
		bits: bitset.New(uint(size)),
	}
}

func (s *mem_store) Set(ctx context.Context, offsets []uint64) {
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, p := range offsets {
		s.bits.Set(uint(p))
	}
}

func (s *mem_store) SetMulti(ctx context.Context, offsetsList [][]uint64) {
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, offsets := range offsetsList {
		for _, p := range offsets {
			s.bits.Set(uint(p))
		}
	}
}

func (s *mem_store) Test(ctx context.Context, offsets []uint64) bool {
	s.lock.RLock()
	defer s.lock.RUnlock()

	for _, p := range offsets {
		if !s.bits.Test(uint(p)) {
			return false
		}
	}

	return true
}

func (s *mem_store) TestMulti(ctx context.Context, offsetsList [][]uint64) []bool {
	s.lock.RLock()
	defer s.lock.RUnlock()

	results := make([]bool, len(offsetsList))
	for i, offsets := range offsetsList {
		exists := true
		for _, p := range offsets {
			if !s.bits.Test(uint(p)) {
				exists = false
				break
			}
		}
		results[i] = exists
	}

	return results
}

func (s *mem_store) Clear(ctx context.Context) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.bits.ClearAll()
}
