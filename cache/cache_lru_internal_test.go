package cache

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// 本文件覆盖 L1 内存缓存 LRU 进化的新增行为：侵入式链表不变量、LRU 驱逐顺序、
// 热 key 豁免边界、并发安全，以及采样游标（SampleKeys + peek）的覆盖性。
// 与 cache_internal_test.go 同包（package cache），可直接访问 mem_store 内部。

// checkListInvariants 断言 mem_store 的内部一致性不变量：
//  1. map 条目数 == 链表节点数（head→tail 正向遍历）
//  2. 双向指针自洽（e.prev.next == e；tail.prev 回链正确）
//  3. 链表中每个 key 都在 map 中，且 map 中每个 key 都可从 head 遍历到达
//  4. usedBytes == Σ len(Value)
//
// 调用方不得同时并发修改 store（测试稳态下调用）。
func checkListInvariants(t *testing.T, s *mem_store) {
	t.Helper()

	s.RLock()
	defer s.RUnlock()

	seen := make(map[string]bool, len(s.items))
	var sumBytes int64
	n := 0
	prev := s.head
	for e := s.head.next; e != s.tail; e = e.next {
		if e.prev != prev {
			t.Fatalf("链表 prev 不自洽：key=%q", e.key)
		}
		if _, ok := s.items[e.key]; !ok {
			t.Fatalf("链表节点 %q 不在 map 中", e.key)
		}
		seen[e.key] = true
		sumBytes += int64(len(e.Value))
		n++
		prev = e
	}
	if s.tail.prev != prev {
		t.Fatalf("tail.prev 回链错误：应为最后一个真实节点")
	}
	if n != len(s.items) {
		t.Fatalf("map 长度 %d != 链表长度 %d", len(s.items), n)
	}
	for k := range s.items {
		if !seen[k] {
			t.Fatalf("map key %q 无法从 head 遍历到达（链表脱链）", k)
		}
	}
	if s.usedBytes != sumBytes {
		t.Fatalf("usedBytes=%d != Σ len(Value)=%d", s.usedBytes, sumBytes)
	}
}

// --- LRU 顺序语义（mem_store 直测）---

func TestMemStoreLRUEvictionOrder(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	s.maxItems = 3
	ctx := context.Background()

	_ = s.Put(ctx, "a", []byte("1"), 0)
	_ = s.Put(ctx, "b", []byte("2"), 0)
	_ = s.Put(ctx, "c", []byte("3"), 0)
	// head→tail: c, b, a

	_, _, _ = s.Get(ctx, "a") // a → MRU：head→tail = a, c, b
	checkListInvariants(t, s)

	_ = s.Put(ctx, "d", []byte("4"), 0) // 超限，逐 LRU 端 b
	checkListInvariants(t, s)

	_, bOK := s.peek("b")
	assert.False(t, bOK, "b（最久未使用）应被 LRU 驱逐")
	for _, k := range []string{"a", "c", "d"} {
		_, ok := s.peek(k)
		assert.True(t, ok, "%s 应保留", k)
	}
	assert.Equal(t, 3, s.Len())
}

func TestMemStoreDeleteKeepsListConsistent(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	ctx := context.Background()

	for _, k := range []string{"u:1", "u:2", "x:1"} {
		_ = s.Put(ctx, k, []byte("v"), 0)
	}
	_, _, _ = s.Get(ctx, "u:1") // 重排，覆盖删除发生在中间节点的情形
	checkListInvariants(t, s)

	_ = s.Delete(ctx, "x:1")
	checkListInvariants(t, s)

	_ = s.DeletePattern(ctx, "u:*")
	checkListInvariants(t, s)
	assert.Equal(t, 0, s.Len())
	assert.Nil(t, s.SampleKeys("", 10))
}

func TestMemStoreExpiredGetRemovesNotPromotes(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	ctx := context.Background()

	_ = s.Put(ctx, "a", []byte("1"), 1) // 1s TTL
	_ = s.Put(ctx, "b", []byte("2"), 0)
	time.Sleep(1100 * time.Millisecond)

	_, exist, _ := s.Get(ctx, "a")
	assert.False(t, exist, "过期 key Get 应 miss")

	// 过期条目被惰性摘除、不参与提升
	checkListInvariants(t, s)
	s.RLock()
	_, ok := s.items["a"]
	s.RUnlock()
	assert.False(t, ok, "过期条目应从 map 摘除")
	assert.Equal(t, []string{"b"}, s.SampleKeys("", 10))
}

// --- 不变量：随机操作序列（Put/Get/GetMulti/SetMulti/Delete/DeletePattern 混合）---

func TestMemStoreRandomizedInvariants(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	s.maxItems = 20
	s.hotKeyThreshold = 2 // 同时压测豁免路径下的链表一致性
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))

	keys := make([]string, 50)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%d", i)
	}

	for i := 0; i < 3000; i++ {
		k := keys[rng.Intn(len(keys))]
		val := []byte(strings.Repeat("v", rng.Intn(12)+1)) // 变长：验证 usedBytes 记账
		switch rng.Intn(7) {
		case 0, 1:
			_ = s.Put(ctx, k, val, 0)
		case 2:
			_, _, _ = s.Get(ctx, k)
		case 3:
			_, _ = s.GetMulti(ctx, keys[rng.Intn(len(keys))], k)
		case 4:
			_ = s.SetMulti(ctx, map[string][]byte{k: val, keys[rng.Intn(len(keys))]: val}, 0)
		case 5:
			_ = s.Delete(ctx, k)
		case 6:
			_ = s.DeletePattern(ctx, "k1*")
		}

		if i%300 == 0 {
			checkListInvariants(t, s)
		}
	}

	checkListInvariants(t, s)
	assert.LessOrEqual(t, s.Len(), 20)
}

// --- 热 key 豁免边界 ---

// 阈值 0（默认）：退化为纯 LRU，无豁免。
func TestHotKeyThresholdZeroIsPureLRU(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	s.maxItems = 3
	// hotKeyThreshold 默认 0
	ctx := context.Background()

	_ = s.Put(ctx, "a", []byte("1"), 0)
	_ = s.Put(ctx, "b", []byte("2"), 0)
	_ = s.Put(ctx, "c", []byte("3"), 0)
	s.Lock()
	s.items["a"].hits = 100 // 即便 a 很热，未开豁免也不应生效
	s.Unlock()
	// head→tail: c, b, a（a 在 tail）
	_ = s.Put(ctx, "d", []byte("4"), 0) // 纯 LRU → 逐 tail=a

	checkListInvariants(t, s)
	_, aOK := s.peek("a")
	assert.False(t, aOK, "豁免关闭时热 key 照常按 LRU 被逐")
}

// 开启豁免：热 key 免逐、冷 key 先逐。
func TestHotKeyExemptionKeepsHotEvictsCold(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	s.maxItems = 3
	s.hotKeyThreshold = 2
	ctx := context.Background()

	_ = s.Put(ctx, "a", []byte("1"), 0)
	_ = s.Put(ctx, "b", []byte("2"), 0)
	_ = s.Put(ctx, "c", []byte("3"), 0)
	// head→tail: c, b, a；把 a 标为热（模拟上周期高频命中，当前漂在 LRU 端）
	s.Lock()
	s.items["a"].hits = 5
	s.Unlock()

	_ = s.Put(ctx, "d", []byte("4"), 0) // len4>3, budget=1：跳过热的 a，逐冷的 b

	checkListInvariants(t, s)
	_, aOK := s.peek("a")
	assert.True(t, aOK, "热 key a 应被豁免保留")
	_, bOK := s.peek("b")
	assert.False(t, bOK, "冷 key b 应先被驱逐")
}

// 超限降级：即便全部条目都够热，len<=maxItems 恒成立且有热条目被逐。
func TestHotKeyExemptionDegradesUnderPressure(t *testing.T) {
	spy := &internalEvictSpy{}
	s := newMemStore()
	s.ttlJitter = 0
	s.maxItems = 10
	s.hotKeyThreshold = 1
	s.metrics = spy
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		k := fmt.Sprintf("k%d", i)
		_ = s.Put(ctx, k, []byte("v"), 0)
		_, _, _ = s.Get(ctx, k) // 让每个 key 都 hits>=1 → 全体"够热"
		assert.LessOrEqual(t, s.Len(), 10, "第 %d 次写入后 len 必须 <= maxItems（降级保证）", i)
	}

	spy.mu.Lock()
	evictions := spy.n
	spy.mu.Unlock()
	checkListInvariants(t, s)
	// 全体够热却发生了驱逐 → 被逐的必是热条目：豁免被预算降级，未撑破容量。
	assert.Greater(t, evictions, 0, "全热场景仍必须发生驱逐（降级），否则容量不变量无法维持")
	assert.LessOrEqual(t, s.Len(), 10)
}

// 计数老化：evictExpired 顺带清零未过期条目的 hits（仅豁免开启时）。
func TestHotKeyHitsClearedEachCycle(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	s.hotKeyThreshold = 1
	ctx := context.Background()

	_ = s.Put(ctx, "a", []byte("1"), 0)
	_, _, _ = s.Get(ctx, "a")
	_, _, _ = s.Get(ctx, "a")
	s.RLock()
	h0 := s.items["a"].hits
	s.RUnlock()
	assert.Equal(t, int64(2), h0)

	s.evictExpired() // 模拟一个清理周期
	s.RLock()
	h1 := s.items["a"].hits
	s.RUnlock()
	assert.Equal(t, int64(0), h1, "清理周期应清零热度（热度窗口 = cleanupInterval）")
}

// 豁免关闭时 evictExpired 不清零（hits 无消费者）。
func TestHitsNotClearedWhenExemptionOff(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	// hotKeyThreshold = 0
	ctx := context.Background()

	_ = s.Put(ctx, "a", []byte("1"), 0)
	_, _, _ = s.Get(ctx, "a")
	s.evictExpired()
	s.RLock()
	h := s.items["a"].hits
	s.RUnlock()
	assert.Equal(t, int64(1), h, "豁免关闭时不清零 hits")
}

// 豁免只免容量驱逐，不免 TTL：热 key 过期照样被清理。
func TestHotKeyExemptionDoesNotPreventTTLExpiry(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	s.maxItems = 1000
	s.hotKeyThreshold = 1
	ctx := context.Background()

	_ = s.Put(ctx, "a", []byte("1"), 1) // 1s TTL
	_, _, _ = s.Get(ctx, "a")
	s.Lock()
	s.items["a"].hits = 100 // 极热
	s.Unlock()
	time.Sleep(1100 * time.Millisecond)

	s.evictExpired() // 后台过期清理：无视热度
	s.RLock()
	_, ok := s.items["a"]
	s.RUnlock()
	assert.False(t, ok, "热 key 过期仍应被后台清理摘除")
	checkListInvariants(t, s)
}

// 豁免不挡显式 Delete。
func TestHotKeyExemptionDoesNotBlockDelete(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	s.hotKeyThreshold = 1
	ctx := context.Background()

	_ = s.Put(ctx, "a", []byte("1"), 0)
	s.Lock()
	s.items["a"].hits = 100
	s.Unlock()

	_ = s.Delete(ctx, "a")
	_, ok := s.peek("a")
	assert.False(t, ok, "显式 Delete 无视热度")
	checkListInvariants(t, s)
}

// maxBytes-only（maxItems=0）时预算仍生效，保证 usedBytes 收敛。
func TestHotKeyExemptionMaxBytesOnlyBudget(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	s.maxItems = 0
	s.maxBytes = 30
	s.hotKeyThreshold = 1
	ctx := context.Background()

	val := []byte("0123456789") // 每项 10 字节 → 上限 3 项
	for i := 0; i < 50; i++ {
		k := fmt.Sprintf("k%d", i)
		_ = s.Put(ctx, k, val, 0)
		_, _, _ = s.Get(ctx, k) // 全热
		s.RLock()
		ub := s.usedBytes
		s.RUnlock()
		assert.LessOrEqual(t, ub, int64(30), "第 %d 次：maxBytes-only 预算须保证 usedBytes 收敛", i)
	}
	checkListInvariants(t, s)
}

// CacheEviction 指标只在实际驱逐时触发，跳过的热条目不计数。
type internalEvictSpy struct {
	mu sync.Mutex
	n  int
}

func (m *internalEvictSpy) CacheEviction()   { m.mu.Lock(); m.n++; m.mu.Unlock() }
func (m *internalEvictSpy) SetDegraded(bool) {}

func TestEvictionMetricCountsOnlyRealEvictions(t *testing.T) {
	spy := &internalEvictSpy{}
	s := newMemStore()
	s.ttlJitter = 0
	s.maxItems = 3
	s.hotKeyThreshold = 2
	s.metrics = spy
	ctx := context.Background()

	_ = s.Put(ctx, "a", []byte("1"), 0)
	_, _, _ = s.Get(ctx, "a")
	_, _, _ = s.Get(ctx, "a") // a 累计 hits=2（此刻仅 a 一个条目，居于 head；随后 b、c 入队会把 a 挤向 tail，见下行）
	_ = s.Put(ctx, "b", []byte("2"), 0)
	_ = s.Put(ctx, "c", []byte("3"), 0)
	// head→tail: c, b, a；a 热且在 tail
	spy.mu.Lock()
	spy.n = 0 // 归零，从此刻开始计量
	spy.mu.Unlock()

	_ = s.Put(ctx, "d", []byte("4"), 0) // 跳过热的 a，逐冷的 b → 1 次驱逐

	spy.mu.Lock()
	got := spy.n
	spy.mu.Unlock()
	assert.Equal(t, 1, got, "跳过的热 key 不计数，只在实际驱逐时 +1")
	checkListInvariants(t, s)
}

// --- 并发安全 ---

func TestMemStoreConcurrentMixedOps(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	s.maxItems = 50       // 小容量：强制持续驱逐
	s.hotKeyThreshold = 2 // 压测豁免路径
	s.cleanupInterval = 5 * time.Millisecond
	s.startEviction()
	defer s.Close()
	ctx := context.Background()

	const keySpace = 300
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				k := fmt.Sprintf("k%d", (g*97+i)%keySpace)
				switch (g + i) % 6 {
				case 0:
					_ = s.Put(ctx, k, []byte("payload"), 0)
				case 1:
					_, _, _ = s.Get(ctx, k)
				case 2:
					_, _ = s.GetMulti(ctx, k)
				case 3:
					_ = s.Delete(ctx, k)
				case 4:
					_, _ = s.peek(k)
				case 5:
					_ = s.SetMulti(ctx, map[string][]byte{k: []byte("x")}, 0)
				}
			}
		}(g)
	}
	wg.Wait()

	checkListInvariants(t, s)
	assert.LessOrEqual(t, s.Len(), 50)
}

// 持续驱逐过程中 Close 不阻塞、不 panic，且幂等。
func TestCloseDuringEvictionDoesNotBlockOrPanic(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	s.maxItems = 10
	s.cleanupInterval = 2 * time.Millisecond
	s.startEviction()
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			_ = s.Put(ctx, fmt.Sprintf("k%d", i), []byte("v"), 0)
		}
	}()

	done := make(chan struct{})
	go func() { s.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close 在持续驱逐过程中被阻塞")
	}

	s.Close() // 二次 Close 幂等
	wg.Wait()
}

// --- 采样游标覆盖性（含 peek 修复饿死）---

// 直接测采样内核：用户读把 hotKey 反复前置 + 采样用 peek（不提升），多轮后
// 游标应覆盖全部 key，冷尾不被饿死。
func TestSampleKeysPeekCoverageNoStarvation(t *testing.T) {
	s := newMemStore()
	s.ttlJitter = 0
	ctx := context.Background()

	total := 20
	keys := make([]string, total)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%d", i)
		_ = s.Put(ctx, keys[i], []byte("v"), 0)
	}

	covered := make(map[string]int)
	cursor := ""
	for round := 0; round < 60; round++ {
		_, _, _ = s.Get(ctx, keys[0]) // 用户读：keys[0] 冲到 MRU
		batch := s.SampleKeys(cursor, 5)
		if len(batch) == 0 {
			cursor = "" // 回绕
			continue
		}
		for _, k := range batch {
			_, ok := s.peek(k) // 采样用 peek：不提升
			assert.True(t, ok)
			covered[k]++
		}
		cursor = batch[len(batch)-1]
	}

	for _, k := range keys {
		assert.Greater(t, covered[k], 0, "key %s 应被采样覆盖（peek 不致冷尾饿死）", k)
	}
}

// 记录被版本同步采样查询到的 key 的远程 store。
type recordingRemoteStore struct {
	mu   sync.Mutex
	seen map[string]int
	data map[string][]byte
}

func newRecordingRemoteStore() *recordingRemoteStore {
	return &recordingRemoteStore{seen: make(map[string]int), data: make(map[string][]byte)}
}

func (r *recordingRemoteStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen[key]++
	v, ok := r.data[key]
	if !ok {
		return nil, false, nil
	}
	return v, true, nil
}

func (r *recordingRemoteStore) Put(_ context.Context, key string, v []byte, _ int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[key] = append([]byte(nil), v...)
	return nil
}

func (r *recordingRemoteStore) Delete(_ context.Context, keys ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range keys {
		delete(r.data, k)
	}
	return nil
}

func (r *recordingRemoteStore) Name() string   { return "recording-remote" }
func (r *recordingRemoteStore) IsRemote() bool { return true }

// 端到端 syncBatch：用户持续读 hotKey 造成重排，多轮 syncBatch（内部走 peek）
// 应覆盖全部 key —— 这正是上一轮"采样用 Get 导致冷尾饿死"缺陷的回归测试。
func TestSyncBatchCoversAllKeysDespiteHotUserReads(t *testing.T) {
	local := newMemStore()
	local.ttlJitter = 0
	remote := newRecordingRemoteStore()
	ctx := context.Background()

	total := 30
	for i := 0; i < total; i++ {
		k := fmt.Sprintf("k%d", i)
		v := makeVersionedData(1, []byte("p"))
		_ = local.Put(ctx, k, v, 0)
		_ = remote.Put(ctx, k, v, 0) // 版本一致 → syncBatch 既不更新也不删除
	}

	c := &cache{
		localStore:       local,
		remoteStore:      remote,
		versionSyncBatch: 5,
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}

	for round := 0; round < 60; round++ {
		_, _, _ = local.Get(ctx, "k0") // 模拟用户密集读，把 k0 反复前置
		c.syncBatch()
	}

	remote.mu.Lock()
	defer remote.mu.Unlock()
	for i := 0; i < total; i++ {
		k := fmt.Sprintf("k%d", i)
		assert.Greater(t, remote.seen[k], 0, "key %s 应被版本同步采样覆盖（冷尾不饿死）", k)
	}
}

// --- hardMode 重扫分支定向覆盖（Minor 1）---

// 现有豁免用例的超限幅度都只有 1~2，预算（budget=len/4）从未耗尽，因此 evictIfNeeded
// 第二阶段的 hardMode 重扫分支（游标抵达 head 后无条件重扫驱逐）从未被触发。
//
// 构造：先用足够大的容量建一批"全热"条目使其共存，再骤然收紧 maxItems——此时
// budget=len/4 > maxItems，第一轮至多跳过 budget 个热条目、其余驱逐后残留仍 >maxItems，
// 游标抵达 head 触发 hardMode，重扫无条件逐出热 key 才收敛到 maxItems。
// 关键判据：最终 len 恰好等于 maxItems（若无 hardMode，会停在 len=budget > maxItems）。
func TestHotKeyExemptionHardModeRescan(t *testing.T) {
	spy := &internalEvictSpy{}
	s := newMemStore()
	s.ttlJitter = 0
	s.hotKeyThreshold = 1 // 全热即可触发豁免路径
	s.metrics = spy
	ctx := context.Background()

	const hot = 60
	originals := make([]string, 0, hot)
	s.maxItems = 1000 // 宽松容量：先让 hot 个热条目共存不被逐
	for i := 0; i < hot; i++ {
		k := fmt.Sprintf("h%d", i)
		_ = s.Put(ctx, k, []byte("v"), 0)
		_, _, _ = s.Get(ctx, k) // 每个 hits>=1 → 全体"够热"
		originals = append(originals, k)
	}
	assert.Equal(t, hot, s.Len(), "构建阶段：热条目应全部共存")

	// budget 归零后开始计量驱逐
	spy.mu.Lock()
	spy.n = 0
	spy.mu.Unlock()

	// 骤然收紧容量：len=60 → maxItems=10，budget=60/4=15 > 10。
	// 一次 Put 触发 evictIfNeeded，单轮无法收敛 → 必须走 hardMode 重扫。
	s.maxItems = 10
	_ = s.Put(ctx, "trigger", []byte("t"), 0)

	// 强判据：容量硬上限被强制维持。无 hardMode 分支时，第一轮扫到 head 会停在
	// len=budget(=15) > maxItems，绝不可能恰好收敛到 10。
	assert.Equal(t, 10, s.Len(), "hardMode 重扫应收敛到 maxItems；若仅停在 budget 残留说明分支未执行")
	assert.LessOrEqual(t, s.Len(), 10, "不变量 len<=maxItems 恒成立")
	checkListInvariants(t, s)

	// 驱逐确实发生，且为收敛到 10 必须逐出大量热条目
	spy.mu.Lock()
	evictions := spy.n
	spy.mu.Unlock()
	assert.Greater(t, evictions, 0, "驱逐应发生")
	assert.GreaterOrEqual(t, evictions, hot-10, "全热降级需逐出足够多的热条目")

	alive := 0
	for _, k := range originals {
		if _, ok := s.peek(k); ok {
			alive++
		}
	}
	assert.Less(t, alive, hot, "hardMode 必然逐出部分原始热 key（豁免被预算降级）")
}

// --- 豁免不挡版本同步清除（Minor 2）---

// 热 key 豁免只免容量驱逐。版本同步（syncBatch）发现"远程已删"时经
// removeFromStorage→Delete 清除本地，该链路无视热度——即便 hits 远超阈值也必须被清。
func TestHotKeyExemptionDoesNotBlockVersionSyncEviction(t *testing.T) {
	local := newMemStore()
	local.ttlJitter = 0
	local.hotKeyThreshold = 1 // 开启豁免
	remote := newRecordingRemoteStore()
	ctx := context.Background()

	v := makeVersionedData(1, []byte("p"))
	_ = local.Put(ctx, "hot", v, 0)
	_ = remote.Put(ctx, "hot", v, 0) // 远程先持有同版本副本

	// 把 "hot" 抬成远超阈值的热 key（同包特权直设 hits）
	local.Lock()
	local.items["hot"].hits = 1000
	local.Unlock()

	// 远程侧删除该 key
	_ = remote.Delete(ctx, "hot")

	c := &cache{
		localStore:       local,
		remoteStore:      remote,
		versionSyncBatch: 10,
		logger:           slog.Default(),
		stopChan:         make(chan struct{}),
	}
	c.syncBatch()

	// 本地 "hot" 应被清除：removeFromStorage→mem_store.Delete 不经豁免路径
	_, ok := local.peek("hot")
	assert.False(t, ok, "热 key 命中'远程已删'仍应被版本同步清除——豁免只免容量驱逐")
	checkListInvariants(t, local)
}
