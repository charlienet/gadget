package cache

import (
	"context"
	"math/rand"
	"path/filepath"
	"sync"
	"time"
)

type mem_store struct {
	items map[string]item
	sync.RWMutex
	stopClean       chan struct{}
	cleanupInterval time.Duration
	closeOnce       sync.Once

	maxItems    int
	maxBytes    int64
	usedBytes   int64
	insertOrder []string

	metrics       Metrics
	ttlJitter     time.Duration // max TTL random jitter applied on Put
	slidingWindow time.Duration // sliding expiration: extend TTL when remaining < this
}

func newMemStore() *mem_store {
	s := &mem_store{
		items:           make(map[string]item),
		stopClean:       make(chan struct{}),
		cleanupInterval: time.Minute,
		metrics:         noopMetrics{},
	}
	return s
}

func (s *mem_store) startEviction() {
	go s.evictLoop()
}

func (s *mem_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	s.RLock()
	i, found := s.items[key]
	s.RUnlock()

	if !found {
		return nil, false, nil
	}

	if i.Expired() {
		s.Lock()
		if i2, ok := s.items[key]; ok && i2.Expired() {
			s.usedBytes -= int64(len(i2.Value))
			delete(s.items, key)
			s.removeFromOrder(key)
		}
		s.Unlock()
		return nil, false, nil
	}

	// Sliding expiration: extend TTL if remaining time < slidingWindow
	if s.slidingWindow > 0 && i.ttlDuration > 0 {
		remaining := i.Expiration - time.Now().UnixNano()
		if remaining < int64(s.slidingWindow) {
			s.Lock()
			if item, ok := s.items[key]; ok && !item.Expired() {
				item.Expiration = time.Now().UnixNano() + item.ttlDuration
				s.items[key] = item
			}
			s.Unlock()
		}
	}

	return i.Value, true, nil
}

func (s *mem_store) Put(ctx context.Context, key string, v []byte, expireSecond int) error {
	ttlDuration := int64(time.Second * time.Duration(expireSecond))

	var e int64
	if expireSecond > 0 {
		e = time.Now().Add(time.Second * time.Duration(expireSecond)).UnixNano()

		// Apply TTL jitter to prevent cache avalanche
		if s.ttlJitter > 0 {
			jitter := time.Duration(rand.Int63n(int64(s.ttlJitter)))
			e += jitter.Nanoseconds()
		}
	}

	s.Lock()
	defer s.Unlock()

	// Remove old value if key exists (for correct byte tracking)
	if old, ok := s.items[key]; ok {
		s.usedBytes -= int64(len(old.Value))
		s.removeFromOrder(key)
	}

	s.items[key] = item{
		Value:       v,
		Expiration:  e,
		ttlDuration: ttlDuration,
	}
	s.usedBytes += int64(len(v))
	s.insertOrder = append(s.insertOrder, key)

	// Evict if over capacity
	s.evictIfNeeded()

	return nil
}

func (s *mem_store) DeletePattern(ctx context.Context, pattern string) error {
	s.Lock()
	defer s.Unlock()

	for k, v := range s.items {
		matched, err := filepath.Match(pattern, k)
		if err != nil {
			continue
		}
		if matched {
			s.usedBytes -= int64(len(v.Value))
			delete(s.items, k)
			s.removeFromOrder(k)
		}
	}
	return nil
}

func (s *mem_store) Delete(ctx context.Context, key ...string) error {
	s.Lock()
	defer s.Unlock()

	for _, k := range key {
		if item, ok := s.items[k]; ok {
			s.usedBytes -= int64(len(item.Value))
			delete(s.items, k)
			s.removeFromOrder(k)
		}
	}

	return nil
}

func (s *mem_store) removeFromOrder(key string) {
	for i, k := range s.insertOrder {
		if k == key {
			s.insertOrder = append(s.insertOrder[:i], s.insertOrder[i+1:]...)
			return
		}
	}
}

func (s *mem_store) evictIfNeeded() {
	now := time.Now().UnixNano()

	// First pass: remove expired entries (free capacity before checking limits)
	for k, v := range s.items {
		if v.Expiration > 0 && now > v.Expiration {
			s.usedBytes -= int64(len(v.Value))
			delete(s.items, k)
			s.removeFromOrder(k)
		}
	}

	// Second pass: FIFO eviction if still over limits
	for (s.maxItems > 0 && len(s.items) > s.maxItems) ||
		(s.maxBytes > 0 && s.usedBytes > s.maxBytes) {
		if len(s.insertOrder) == 0 {
			break
		}
		oldest := s.insertOrder[0]
		s.insertOrder = s.insertOrder[1:]
		if item, ok := s.items[oldest]; ok {
			s.usedBytes -= int64(len(item.Value))
			delete(s.items, oldest)
		}
		s.metrics.CacheEviction()
	}
}

func (s *mem_store) GetMulti(ctx context.Context, keys ...string) (map[string][]byte, error) {
	result := make(map[string][]byte, len(keys))

	s.RLock()
	defer s.RUnlock()

	now := time.Now().UnixNano()
	for _, key := range keys {
		if item, ok := s.items[key]; ok {
			if item.Expiration == 0 || now <= item.Expiration {
				result[key] = item.Value
			}
		}
	}

	return result, nil
}

func (s *mem_store) SetMulti(ctx context.Context, items map[string][]byte, expireSecond int) error {
	ttlDuration := int64(time.Second * time.Duration(expireSecond))

	s.Lock()
	defer s.Unlock()

	for key, val := range items {
		var e int64
		if expireSecond > 0 {
			e = time.Now().Add(time.Second * time.Duration(expireSecond)).UnixNano()

			// 与单键 Put 一致：每 key 独立应用 TTL jitter，防止批量写入后同时过期
			if s.ttlJitter > 0 {
				jitter := time.Duration(rand.Int63n(int64(s.ttlJitter)))
				e += jitter.Nanoseconds()
			}
		}

		if old, ok := s.items[key]; ok {
			s.usedBytes -= int64(len(old.Value))
			s.removeFromOrder(key)
		}

		s.items[key] = item{
			Value:       val,
			Expiration:  e,
			ttlDuration: ttlDuration,
		}
		s.usedBytes += int64(len(val))
		s.insertOrder = append(s.insertOrder, key)
	}

	s.evictIfNeeded()

	return nil
}

// SampleKeys returns up to n keys from the insert order, starting from the
// given offset (for iterating across the key space without locking for long).
func (s *mem_store) SampleKeys(offset, n int) []string {
	s.RLock()
	defer s.RUnlock()

	if offset >= len(s.insertOrder) {
		return nil
	}
	end := offset + n
	if end > len(s.insertOrder) {
		end = len(s.insertOrder)
	}
	keys := make([]string, end-offset)
	copy(keys, s.insertOrder[offset:end])
	return keys
}

func (s *mem_store) Len() int {
	s.RLock()
	defer s.RUnlock()
	return len(s.items)
}

func (*mem_store) IsRemote() bool { return false }

func (*mem_store) Name() string { return "memory" }

// Close 停止后台清理 goroutine。幂等：两个 cache 实例共享同一 store 时
// 二次 Close 不 panic。
func (s *mem_store) Close() {
	s.closeOnce.Do(func() {
		close(s.stopClean)
	})
}

func (s *mem_store) evictLoop() {
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.evictExpired()
		case <-s.stopClean:
			return
		}
	}
}

func (s *mem_store) evictExpired() {
	now := time.Now().UnixNano()
	s.Lock()
	defer s.Unlock()
	for k, v := range s.items {
		if v.Expiration > 0 && now > v.Expiration {
			s.usedBytes -= int64(len(v.Value))
			delete(s.items, k)
			s.removeFromOrder(k)
		}
	}
}

type item struct {
	Value       []byte
	Expiration  int64
	ttlDuration int64 // original TTL in nanoseconds (for sliding expiration)
}

func (i *item) Expired() bool {
	if i.Expiration == 0 {
		return false
	}

	return time.Now().UnixNano() > i.Expiration
}
