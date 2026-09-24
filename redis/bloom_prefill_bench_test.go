package redis

// bloom prefill 门控热路径开销基准：回答"启用 WithPrefill 后对 Ready 态
// 数据面有多少开销"。
//
// 对照口径（同一真单机 Redis 环境，gate 同 benchStandaloneClient，缺
// REDIS_URL 即 b.Skip）：
//   - BenchmarkBloomPlainAdd / BenchmarkBloomPlainExists：未启用 WithPrefill
//     的工厂产物（inner 裸路径，bf 模块在场走 bfCmdImpl、否则 bitmapImpl）；
//   - BenchmarkBloomPrefillReadyAdd / BenchmarkBloomPrefillReadyExists：启用
//     WithPrefill（空回灌 fn、默认 syncInterval=1s）的装饰器，预热至本地
//     新鲜 Ready 后计时。
//   两组均经 NewBloomFilter 工厂构造（同容量 1M / FPR 0.01，与
//   benchNewFilter 口径一致），唯一差异 = 是否包装 prefillFilter 装饰器，
//   差异即门控开销本身。
//
// 热路径预期：Ready 态 Add = maybeTrigger（loadLocal atomic.Pointer 读 +
// 相位短路，零 RTT）+ 原命令；Exists = maybeTrigger + readyFresh（再一次
// atomic 读 + time.Since）+ 原命令。纳秒级本地开销叠加在 ~100µs 网络 RTT
// 上，判读预期差异在噪声级（<5-10%）；若 >20% 视为异常，停下报告不修。
//
// 同步税说明（规格第 3 项，观测口径注释）：prefill 的后台成本——loop 每
// syncInterval（默认 1s）一次状态键 GET、Failed 冷却计数——发生在协调器
// goroutine，不在 benchmark 计时循环的调用栈里，**不体现在单请求 ns/op
// 中**；本组数字只度量热路径判定（2 次 atomic 读 + time.Since）+ 数据面
// 命令。若要量化同步税对尾延迟的影响，可在同结构 Exists 基准上改
// WithSyncInterval 缩短间隔（GET 更密、stale 阈值 2×间隔同步收紧），
// 观察 ns/op 与 p99 的漂移——默认 1s 间隔下 GET 摊薄到每秒一次，
// 对单请求延迟的影响在本基准分辨率（benchtime=200ms×count=3）下不可见。
//
// 纪律（沿用 bloom_bench_test.go）：键 bench: 前缀 + 随机后缀；清理只经
// rc.Del 逐键删除（禁多 key 合成 DEL 依赖、禁 FLUSHDB）；benchtime 由
// 运行命令行控制，代码不设；预热/预灌等重准备在 b.StopTimer 隔离计时。

import (
	"context"
	"testing"
	"time"
)

// benchPlainFilter 白盒外口径的对照组：工厂直构未启用 WithPrefill 的
// BloomFilter（能力分派与实验组同 server 同结果），容量参数对齐既有
// bench（1M / 0.01），键清理沿用 benchCleanupKeys。
func benchPlainFilter(b *testing.B, rc *redisClient, suffix string) BloomFilter {
	b.Helper()
	ctx := context.Background()
	f, err := rc.NewBloomFilter(ctx, benchKey("plain-"+suffix),
		WithCapacity(1_000_000),
		WithFalsePositive(0.01),
	)
	if err != nil {
		b.Fatalf("NewBloomFilter: %v", err)
	}
	benchCleanupKeys(b, rc, f) //nolint:contextcheck // bench 清理闭包：testing.B 无 Context()，删除须脱离计时/上下文取消
	return f
}

// benchPrefillReadyFilter 组装启用 WithPrefill 的 *prefillFilter 并预热至
// 本地新鲜 Ready：空回灌 fn（基准计时区内只跑数据面操作）、默认
// syncInterval=1s（生产缺省形态）；构造后由后台 loop 自动完成冷启动
// force=0 重建（jitter[0,1s) + Δ=2s 等待 + inner.Reset + 空 fn → ready），
// 等待在计时外。
func benchPrefillReadyFilter(b *testing.B, rc *redisClient, suffix string) *prefillFilter {
	b.Helper()
	ctx := context.Background()
	f, err := rc.NewBloomFilter(ctx, benchKey("prefill-"+suffix),
		WithCapacity(1_000_000),
		WithFalsePositive(0.01),
		WithPrefill(func(context.Context, PrefillIngest) error { return nil }),
	)
	if err != nil {
		b.Fatalf("NewBloomFilter(WithPrefill): %v", err)
	}
	pf, ok := f.(*prefillFilter)
	if !ok {
		b.Fatalf("启用预填充的工厂产物应为 *prefillFilter，got %T", f)
	}

	// 清理：数据键（inner 白盒 sharder 口径逐键）+ 状态键 + failn 键，
	// 全部 rc.Del。注册顺序（LIFO）：先注册删键、再注册 pf.Close——执行
	// 时先停后台 loop 再删键，防 loop 在删键后同步触发重建写回状态键。
	var keys []string
	switch inner := pf.inner.(type) {
	case *bfCmdImpl:
		keys = inner.sharder.allKeys()
	case *bitmapImpl:
		keys = inner.sharder.allKeys()
	default:
		b.Fatalf("unknown prefill inner %T", inner)
	}
	keys = append(keys, pf.coord.stateKey, pf.coord.failnKey)
	b.Cleanup(func() {
		cctx := context.Background()
		for _, k := range keys {
			_ = rc.Del(cctx, k).Err()
		}
	}) //nolint:contextcheck // bench 清理闭包：testing.B 无 Context()，删除须脱离计时/上下文取消
	b.Cleanup(func() { _ = pf.Close() })

	// 预热：等本地相位达到新鲜 Ready（run finish 的 storeLocal 与后续
	// loop 刷新共同维持 fresh）；超时说明冷启动链路断裂，直接 FailNow。
	deadline := time.Now().Add(20 * time.Second)
	for !pf.coord.readyFresh() {
		if time.Now().After(deadline) {
			b.Fatalf("prefill 未在 20s 内到达新鲜 Ready（本地相位 %v）",
				pf.coord.loadLocal().phase)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return pf
}

// BenchmarkBloomPlainAdd 对照组：未启用预填充的单条 Add。
func BenchmarkBloomPlainAdd(b *testing.B) {
	rc, _ := benchStandaloneClient(b)
	f := benchPlainFilter(b, rc, "add")
	ctx := context.Background()
	seq := &benchSeq{tag: benchKey("item")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := f.Add(ctx, seq.next()); err != nil {
			b.Fatalf("Add: %v", err)
		}
	}
}

// BenchmarkBloomPlainExists 对照组：未启用预填充的单条 Exists
// （预灌 1000 item 池轮转，沿用既有 bench 口径）。
func BenchmarkBloomPlainExists(b *testing.B) {
	rc, _ := benchStandaloneClient(b)
	f := benchPlainFilter(b, rc, "exists")
	ctx := context.Background()
	pool := (&benchSeq{tag: benchKey("pool")}).batch(1000)
	b.StopTimer()
	if _, err := f.AddMulti(ctx, pool...); err != nil {
		b.Fatalf("预灌入 AddMulti: %v", err)
	}
	b.StartTimer()
	b.ReportAllocs()
	var j int // pool 轮转下标（与 i%len(pool) 等价）
	for b.Loop() {
		if _, err := f.Exists(ctx, pool[j%len(pool)]); err != nil {
			b.Fatalf("Exists: %v", err)
		}
		j++
	}
}

// BenchmarkBloomPrefillReadyAdd 实验组：Ready 态装饰器单条 Add
// （maybeTrigger 短路 + inner.Add）。
func BenchmarkBloomPrefillReadyAdd(b *testing.B) {
	rc, _ := benchStandaloneClient(b)
	f := benchPrefillReadyFilter(b, rc, "add")
	ctx := context.Background()
	seq := &benchSeq{tag: benchKey("item")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := f.Add(ctx, seq.next()); err != nil {
			b.Fatalf("Add: %v", err)
		}
	}
}

// BenchmarkBloomPrefillReadyExists 实验组：Ready 态装饰器单条 Exists
// （maybeTrigger 短路 + readyFresh 一次 atomic 读 + inner.Exists 真实查询）。
// 后台每秒状态 GET 的同步税不在本循环调用栈内，见文件头注释。
func BenchmarkBloomPrefillReadyExists(b *testing.B) {
	rc, _ := benchStandaloneClient(b)
	f := benchPrefillReadyFilter(b, rc, "exists")
	ctx := context.Background()
	pool := (&benchSeq{tag: benchKey("pool")}).batch(1000)
	b.StopTimer()
	if _, err := f.AddMulti(ctx, pool...); err != nil {
		b.Fatalf("预灌入 AddMulti: %v", err)
	}
	b.StartTimer()
	b.ReportAllocs()
	var j int // pool 轮转下标（与 i%len(pool) 等价）
	for b.Loop() {
		if _, err := f.Exists(ctx, pool[j%len(pool)]); err != nil {
			b.Fatalf("Exists: %v", err)
		}
		j++
	}
}
