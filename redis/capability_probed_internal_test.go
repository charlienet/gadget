package redis

import (
	"testing"
)

// v0.11.0 G4：Probed() 探测状态查询（capability.go）。
//
// 离线部分用 miniredis 覆盖「初始 false / 探测失败保持 false / 重探失败
// 失效旧置位 / Refresh 清 false」四段状态迁移（miniredis 不支持带
// section 的 INFO，Probe 必失败——正是"探测失败不得置位/不得保留旧
// 置位"的判据）；「全成功置 true」须真 Redis（gate 对齐包内既有
// REDIS_URL 纪律）。

// TestCapabilityProbedStateMachine 离线状态机：初始 false → Probe 失败
// 保持 false → 白盒置位后重探失败回落 false → 再置位后 Refresh 清 false。
func TestCapabilityProbedStateMachine(t *testing.T) {
	ctx := t.Context()
	rc, _ := newMiniRedisClient(t)

	if rc.cap.Probed() {
		t.Fatal("新建客户端 Probed() 应为 false（未探测）")
	}

	// miniredis 不支持 INFO <section>：Probe 返回错误，且失败不得置位
	// （对齐「不得把探测失败当不支持缓存」口径）。
	if err := rc.cap.Probe(ctx); err == nil {
		t.Fatal("miniredis 上 Probe 应失败（不支持带 section 的 INFO）")
	}
	if rc.cap.Probed() {
		t.Fatal("探测失败后 Probed() 应保持 false，不得置位")
	}

	// 白盒模拟「上次探测全成功」→ 重探失败（miniredis 不支持 INFO
	// section，Probe 必报错）→ probeLocked 开头复位使旧置位失效，回落
	// false（ISSUE-203：锁定复位行，删除该行本用例必须变红）。
	setProbedForTest(t, rc.cap, true, []moduleInfo{{Name: "bf"}}, false)
	if !rc.cap.Probed() {
		t.Fatal("白盒置位后 Probed() 应为 true")
	}
	if err := rc.cap.Probe(ctx); err == nil {
		t.Fatal("miniredis 上重探应失败（不支持带 section 的 INFO）")
	}
	if rc.cap.Probed() {
		t.Fatal("重探失败必须使旧置位失效回落 false，不得停留 true（复位行失效）")
	}

	// 白盒再次置位「已全成功」→ Refresh 清置位（回到未探测保守态）。
	setProbedForTest(t, rc.cap, true, nil, false)
	if !rc.cap.Probed() {
		t.Fatal("白盒置位后 Probed() 应为 true")
	}
	rc.cap.Refresh()
	if rc.cap.Probed() {
		t.Fatal("Refresh 后 Probed() 应清回 false")
	}
}

// TestCapabilityProbedRealRedisFullSuccess 真 Redis（gate：REDIS_URL 未
// 设置或无 bf 模块即 Skip）：Probe 全部步骤成功 → Probed() 置 true；
// 随后 Refresh 清回 false。
func TestCapabilityProbedRealRedisFullSuccess(t *testing.T) {
	ctx := t.Context()
	rc := newRealStandaloneClient(t)

	if err := rc.Capability().Probe(ctx); err != nil {
		t.Fatalf("真 Redis Probe: %v", err)
	}
	if !rc.Capability().Probed() {
		t.Fatal("Probe 全部成功后 Probed() 应为 true")
	}
	if !rc.Capability().HasBloom() {
		t.Skip("server has no bf module; skip probed-true 口径确认")
	}

	rc.Capability().Refresh()
	if rc.Capability().Probed() {
		t.Fatal("Refresh 后 Probed() 应清回 false（重探须显式 Probe）")
	}
}

// TestCapabilityProbedConcurrent -race 并发口径：多 goroutine 并发调
// Probed()/Refresh()/HasBloom()（持锁纯内存读）不得触发 race 检测。
func TestCapabilityProbedConcurrent(t *testing.T) {
	c := &Capability{}
	done := make(chan struct{})
	for i := range 8 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			switch i % 4 {
			case 0:
				_ = c.Probed()
			case 1:
				_ = c.HasBloom()
			case 2:
				c.Refresh()
			case 3:
				setProbedForTest(t, c, true, []moduleInfo{{Name: "bf"}}, false)
			}
		}(i)
	}
	for range 8 {
		<-done
	}
}
