// Package cache 的 L1 内存存储实现。
//
// usedBytes 口径：仅累计各条目 len(Value)（序列化后的值字节数），不含 key 字符串、
// entry 结构或 map 开销。容量驱逐（maxBytes）与 TestCapacityMaxBytes 的精确字节
// 算术都依赖此口径，任何改动须保持"覆写扣旧加新、删除即减"的一致性。
package cache

import (
	"context"
	"math/rand"
	"path/filepath"
	"sync"
	"time"
)

// entry 是 mem_store 的存储单元：内嵌 item（Value/Expiration/ttlDuration，字段提升
// 使 e.Value、e.Expiration 等直接可用），并携带 LRU 侵入式双向链表指针与热度过计数。
type entry struct {
	key  string
	item        // 内嵌：Value []byte / Expiration int64 / ttlDuration int64
	hits int64  // 本清理周期内的命中次数（热 key 豁免判定用）
	prev *entry // 指向前一节点（朝向 head / 最近使用端）
	next *entry // 指向后一节点（朝向 tail / 最久未使用端）
}

// mem_store 是 L1 内存缓存：map 索引 + 侵入式双向链表维护 LRU 顺序。
//
// 链表约定：head 哨兵之后为最近使用（MRU），tail 哨兵之前为最久未使用（LRU）；
// 容量驱逐从 tail 端向前取。head.prev 与 tail.next 恒为 nil，其余节点（含哨兵
// 参与的连接）的 prev/next 在链表内均非 nil，故 remove/moveToFront/pushFront 可直接
// 解引用无需判空。所有指针变更必须经由这三个助手，禁止在业务代码中手写 prev/next。
type mem_store struct {
	items map[string]*entry
	sync.RWMutex
	stopClean       chan struct{}
	cleanupInterval time.Duration
	closeOnce       sync.Once

	maxItems        int
	maxBytes        int64
	usedBytes       int64 // 见文件顶部：仅 Σ len(Value)
	hotKeyThreshold int   // >= 1 开启热 key 豁免；<= 0 关闭（默认）

	head *entry // MRU 端哨兵
	tail *entry // LRU 端哨兵

	metrics       Metrics
	ttlJitter     time.Duration // max TTL random jitter applied on Put
	slidingWindow time.Duration // sliding expiration: extend TTL when remaining < this
}

func newMemStore() *mem_store {
	s := &mem_store{
		items:           make(map[string]*entry),
		stopClean:       make(chan struct{}),
		cleanupInterval: time.Minute,
		metrics:         noopMetrics{},
	}
	// 初始化哨兵：head <-> tail，空表。
	s.head = &entry{}
	s.tail = &entry{}
	s.head.next = s.tail
	s.tail.prev = s.head
	return s
}

func (s *mem_store) startEviction() {
	go s.evictLoop()
}

// pushFront 把尚未入链的 e 插入到 head 之后（成为最新使用节点）。
// 调用方须持有写锁。
func (s *mem_store) pushFront(e *entry) {
	e.prev = s.head
	e.next = s.head.next
	s.head.next.prev = e
	s.head.next = e
}

// moveToFront 把已在链中的 e 移到 head 之后（更新为最新使用）。调用方须持写锁。
func (s *mem_store) moveToFront(e *entry) {
	s.remove(e)
	s.pushFront(e)
}

// remove 把 e 从双向链表摘除（不改动 map，也不释放 e）。调用方须持写锁。
func (s *mem_store) remove(e *entry) {
	e.prev.next = e.next
	e.next.prev = e.prev
}

func (s *mem_store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	// LRU 提升需写链表，故全程持写锁单阶段处理（不再 RLock 读→Lock 复检）。
	s.Lock()
	defer s.Unlock()

	e, found := s.items[key]
	if !found {
		return nil, false, nil
	}

	if e.Expired() {
		// 惰性过期：不提升，直接摘除。
		s.usedBytes -= int64(len(e.Value))
		s.remove(e)
		delete(s.items, key)
		return nil, false, nil
	}

	e.hits++
	s.moveToFront(e)

	// Sliding expiration: extend TTL if remaining time < slidingWindow
	if s.slidingWindow > 0 && e.ttlDuration > 0 {
		remaining := e.Expiration - time.Now().UnixNano()
		if remaining < int64(s.slidingWindow) {
			e.Expiration = time.Now().UnixNano() + e.ttlDuration
		}
	}

	return e.Value, true, nil
}

// peek 读取 key 的当前值，不做 LRU 提升、不增加热度计数，仅可在过期时惰性摘除。
// 专供版本同步采样（syncBatch）使用：采样不得扰动 LRU 顺序，否则游标会在 head 端
// 簇震荡、tail 端冷 key 被长期饿死。非导出，仅供 package cache 内部调用。
func (s *mem_store) peek(key string) ([]byte, bool) {
	s.Lock()
	defer s.Unlock()

	e, found := s.items[key]
	if !found {
		return nil, false
	}
	if e.Expired() {
		s.usedBytes -= int64(len(e.Value))
		s.remove(e)
		delete(s.items, key)
		return nil, false
	}
	return e.Value, true
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

	if old, ok := s.items[key]; ok {
		// 覆写：原地更新 entry.item（不重新分配节点），字节精确记账（扣旧加新），
		// 并提升到 MRU。保留 old.hits（热度不因覆写清零，老化只在清理周期发生）。
		s.usedBytes += int64(len(v)) - int64(len(old.Value))
		old.item = item{Value: v, Expiration: e, ttlDuration: ttlDuration}
		s.moveToFront(old)
	} else {
		ne := &entry{key: key, item: item{Value: v, Expiration: e, ttlDuration: ttlDuration}}
		s.items[key] = ne
		s.usedBytes += int64(len(v))
		s.pushFront(ne)
	}

	// Evict if over capacity
	s.evictIfNeeded()

	return nil
}

func (s *mem_store) DeletePattern(ctx context.Context, pattern string) error {
	s.Lock()
	defer s.Unlock()

	for k, e := range s.items {
		matched, err := filepath.Match(pattern, k)
		if err != nil {
			continue
		}
		if matched {
			s.usedBytes -= int64(len(e.Value))
			s.remove(e)
			delete(s.items, k)
		}
	}
	return nil
}

func (s *mem_store) Delete(ctx context.Context, key ...string) error {
	s.Lock()
	defer s.Unlock()

	for _, k := range key {
		if e, ok := s.items[k]; ok {
			s.usedBytes -= int64(len(e.Value))
			s.remove(e)
			delete(s.items, k)
		}
	}

	return nil
}

func (s *mem_store) evictIfNeeded() {
	// 调用方须持写锁。
	now := time.Now().UnixNano()

	// First pass: remove expired entries (free capacity before checking limits).
	// O(1) 摘节点；行为对齐旧实现，不做性能优化。
	for k, e := range s.items {
		if e.Expiration > 0 && now > e.Expiration {
			s.usedBytes -= int64(len(e.Value))
			s.remove(e)
			delete(s.items, k)
		}
	}

	// Second pass: capacity eviction from the LRU (tail) end.
	//
	// 热 key 豁免（hotKeyThreshold > 0 时）：从 tail 向前，hits >= threshold 的条目
	// 本轮跳过，但最多跳过 budget = max(1, len(items)/4) 个——达到预算后不再豁免，
	// 一律驱逐（降级），保证 len 始终收敛到容量上限。豁免只作用于容量驱逐，
	// 不影响第一阶段的过期清理。若一轮扫描结束（游标抵达 head）仍超限，则进入
	// 硬驱逐模式重扫，此时无视豁免，确保不变量 len <= maxItems 恒成立。
	budget := len(s.items) / 4
	if budget < 1 {
		budget = 1
	}
	skips := 0
	hardMode := false
	cur := s.tail.prev

	for (s.maxItems > 0 && len(s.items) > s.maxItems) ||
		(s.maxBytes > 0 && s.usedBytes > s.maxBytes) {
		if cur == s.head {
			if hardMode {
				break // 表已空仍超限（理论上不可达）——防御性退出
			}
			hardMode = true
			cur = s.tail.prev
			continue
		}
		if !hardMode && s.hotKeyThreshold > 0 &&
			cur.hits >= int64(s.hotKeyThreshold) && skips < budget {
			skips++
			cur = cur.prev
			continue
		}

		victim := cur
		cur = cur.prev // 先移动游标，避免 remove 后 victim 指针失效
		s.usedBytes -= int64(len(victim.Value))
		s.remove(victim)
		delete(s.items, victim.key)
		s.metrics.CacheEviction()
	}
}

func (s *mem_store) GetMulti(ctx context.Context, keys ...string) (map[string][]byte, error) {
	result := make(map[string][]byte, len(keys))

	s.Lock()
	defer s.Unlock()

	now := time.Now().UnixNano()
	for _, key := range keys {
		if e, ok := s.items[key]; ok {
			if e.Expiration == 0 || now <= e.Expiration {
				e.hits++
				s.moveToFront(e)
				result[key] = e.Value
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
			s.usedBytes += int64(len(val)) - int64(len(old.Value))
			old.item = item{Value: val, Expiration: e, ttlDuration: ttlDuration}
			s.moveToFront(old)
		} else {
			ne := &entry{key: key, item: item{Value: val, Expiration: e, ttlDuration: ttlDuration}}
			s.items[key] = ne
			s.usedBytes += int64(len(val))
			s.pushFront(ne)
		}
	}

	s.evictIfNeeded()

	return nil
}

// SampleKeys 返回 LRU 遍历序（head→tail，即从最近使用向最久使用）上、afterKey
// 之后至多 n 个 key，供版本同步游标式采样。afterKey 为空串或不存在（被驱逐/首轮
// 回绕）时从 head 之后重新开始。采样为纯读，绝不提升任何 key。
//
// 语义变化：旧版按插入顺序 offset 切片；新版基于 LRU 链表，故起始位置由 key 决定，
// 对驱逐导致的下标漂移不敏感。调用方（syncBatch）以"返回空批 → 重置游标"实现回绕。
func (s *mem_store) SampleKeys(afterKey string, n int) []string {
	s.RLock()
	defer s.RUnlock()

	if n <= 0 {
		return nil
	}

	cur := s.head
	if afterKey != "" {
		if e, ok := s.items[afterKey]; ok {
			cur = e
		}
	}

	keys := make([]string, 0, n)
	for e := cur.next; e != s.tail && len(keys) < n; e = e.next {
		keys = append(keys, e.key)
	}
	if len(keys) == 0 {
		return nil
	}
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

// evictExpired 是后台清理周期的入口：摘除已过期条目，并顺带对未过期条目做热度
// 老化（hits 清零）。热度窗口因此等于 cleanupInterval——"最近一个清理周期内的命中"
// 决定热 key 身份，跨周期不累计。清零仅在热 key 豁免开启时执行（关闭时 hits 无消费者）。
func (s *mem_store) evictExpired() {
	now := time.Now().UnixNano()
	s.Lock()
	defer s.Unlock()
	clearHits := s.hotKeyThreshold > 0
	for k, e := range s.items {
		if e.Expiration > 0 && now > e.Expiration {
			s.usedBytes -= int64(len(e.Value))
			s.remove(e)
			delete(s.items, k)
			continue
		}
		if clearHits {
			e.hits = 0
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
