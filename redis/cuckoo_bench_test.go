package redis

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Cuckoo Benchmark 矩阵：路径（cfCmdImpl 原生 / hashImpl 降级）× 环境
// （真单机 REDIS_URL / 真集群 REDIS_CLUSTER）× 操作（Add/Exists/Del/AddNX/
// Count/ExistsMulti/AddMulti/Info/Reset）。
//
// 纪律：
//   - 缺环境变量 → b.Skip（不设强制门槛，CI 无真环境时全绿跳过）；
//   - key 前缀 bench:cf: / bench:hash: + UnixNano 唯一段，b.Cleanup 逐 key
//     DEL；严禁 FLUSHDB；
//   - 预灌/清场等准备阶段一律 b.StopTimer/b.StartTimer 包住；
//   - 代码内不设 benchtime（由外部 -benchtime 控制）。建议 -benchtime=100x
//     起步：Del/AddMulti 类每次迭代消耗或产出成批条目，auto 放大 benchtime
//     时可能耗尽预灌预算（注释标 per-op 预算上限）。
//   - Multi 类 size 取 100/1000；10000 档评估后降档未实现——单次 EVAL
//     万级 item 在 hashImpl 上 O(n×maxIterations×bucketSize) 与集群 RTT
//     预算下耗时失控，且与 100x 迭代的累计容量冲突（详见任务报告）。
//
// 白盒直构说明：hashImpl 在带模块服务器上经工厂 auto 分派物理不可达
// （见 cuckoo_internal_test.go 缺口注释），bench 与真环境用例同法直构；
// cfCmdImpl 手动 Store 闸门（构造纪律与 NewCuckooFilter 一致）。
// ---------------------------------------------------------------------------

// benchCuckooClient 从环境变量取 URL 建连；缺失时 b.Skip。
func benchCuckooClient(b *testing.B, env string) *redisClient {
	b.Helper()
	raw := os.Getenv(env)
	if raw == "" {
		b.Skipf("%s not set; skip real-env benchmark", env)
	}
	rdb, err := NewWithUrl(raw)
	if err != nil {
		b.Fatalf("%s URL 解析失败：%v", env, err)
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		b.Fatalf("unexpected client type %T", rdb)
	}
	b.Cleanup(func() { _ = rc.GracefulClose(context.Background()) })
	return rc
}

// benchCuckooImpl 在指定环境与路径上跑全操作矩阵。
func benchCuckooImpl(b *testing.B, env, pathTag string,
	mk func(rc *redisClient, key string, cfg cuckooConfig) cuckooFilterImpl,
) {
	b.Helper()
	rc := benchCuckooClient(b, env)
	ctx := context.Background()

	cfg := defaultCuckooConfig()
	cfg.capacity = 2_000_000 // 全操作共用容量预算（预灌+迭代累计的余量上限）

	// 每个 sub-bench 独享新 key（互不污染）+ 注册 Cleanup 删除。
	newImpl := func(b *testing.B, suffix string) cuckooFilterImpl {
		b.Helper()
		key := fmt.Sprintf("bench:%s:%d:%s", pathTag, time.Now().UnixNano(), suffix)
		impl := mk(rc, key, cfg)
		b.Cleanup(func() { _ = rc.Del(context.Background(), key).Err() })
		// 预热 RESERVE/首 EVAL（模块版闸门），把一次性建过滤器开销挡在计时外。
		if c, ok := impl.(*cfCmdImpl); ok {
			b.StopTimer()
			if err := c.ensureReserve(ctx); err != nil {
				b.Fatalf("预热 CF.RESERVE：%v", err)
			}
			b.StartTimer()
		}
		return impl
	}

	// benchSeed 经 AddMulti 分批预灌 n 条（prefix-i 形态，与 probe 前缀不相交）。
	benchSeed := func(b *testing.B, impl cuckooFilterImpl, prefix string, n int) {
		b.Helper()
		for off := 0; off < n; off += 1000 {
			end := min(off+1000, n)
			items := make([]any, 0, end-off)
			for i := off; i < end; i++ {
				items = append(items, fmt.Sprintf("%s-%d", prefix, i))
			}
			res, err := impl.AddMulti(ctx, items...)
			if err != nil {
				b.Fatalf("预灌 %s：%v", prefix, err)
			}
			for j, v := range res {
				if !v {
					b.Fatalf("预灌 %s 第 %d 项失败（容量预算不足？）", prefix, off+j)
				}
			}
		}
	}

	b.Run("Add", func(b *testing.B) {
		impl := newImpl(b, "add") // 空键稳态，逐条新增
		b.ReportAllocs()
		for i := range b.N {
			if _, err := impl.Add(ctx, fmt.Sprintf("a-%d", i)); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Exists", func(b *testing.B) {
		impl := newImpl(b, "ex")
		b.StopTimer()
		benchSeed(b, impl, "ex", 1000)
		b.StartTimer()
		b.ReportAllocs()
		for i := range b.N {
			if _, err := impl.Exists(ctx, fmt.Sprintf("ex-%d", i%1000)); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Del", func(b *testing.B) {
		impl := newImpl(b, "del")
		b.StopTimer()
		benchSeed(b, impl, "del", 20_000) // 预算上限：b.N ≤ 20000（-benchtime 100x/1000x 安全）
		b.StartTimer()
		b.ReportAllocs()
		for i := range b.N {
			if _, err := impl.Del(ctx, fmt.Sprintf("del-%d", i)); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("AddNX", func(b *testing.B) {
		impl := newImpl(b, "nx")
		b.ReportAllocs()
		for i := range b.N {
			if _, err := impl.AddNX(ctx, fmt.Sprintf("nx-%d", i)); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Count", func(b *testing.B) {
		impl := newImpl(b, "cnt")
		b.StopTimer()
		benchSeed(b, impl, "cnt", 1000)
		b.StartTimer()
		b.ReportAllocs()
		for i := range b.N {
			if _, err := impl.Count(ctx, fmt.Sprintf("cnt-%d", i%1000)); err != nil {
				b.Fatal(err)
			}
		}
	})

	// ---- 批量类：size 100 / 1000 ----
	for _, size := range []int{100, 1000} {
		b.Run(fmt.Sprintf("ExistsMulti/%d", size), func(b *testing.B) {
			impl := newImpl(b, "mx")
			b.StopTimer()
			benchSeed(b, impl, "mx", size)
			starters := make([][]any, 8) // 轮换 8 组入参，规避同参缓存效应
			for g := range starters {
				q := make([]any, size)
				for j := range q {
					q[j] = fmt.Sprintf("mx-%d", (j+g)%size)
				}
				starters[g] = q
			}
			b.StartTimer()
			b.ReportAllocs()
			for i := range b.N {
				if _, err := impl.ExistsMulti(ctx, starters[i%len(starters)]...); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("AddMulti/%d", size), func(b *testing.B) {
			impl := newImpl(b, "mm")
			b.ReportAllocs()
			items := make([]any, size)
			for i := range b.N {
				for j := range items {
					items[j] = fmt.Sprintf("mm-%d-%d", i, j)
				}
				if _, err := impl.AddMulti(ctx, items...); err != nil {
					b.Fatal(err)
				}
			}
		})
	}

	b.Run("Info", func(b *testing.B) {
		impl := newImpl(b, "info")
		b.StopTimer()
		benchSeed(b, impl, "inf", 1) // 确保键存在（模块版 CF.INFO 对缺失键报错）
		b.StartTimer()
		b.ReportAllocs()
		for range b.N {
			if _, err := impl.Info(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Reset", func(b *testing.B) {
		impl := newImpl(b, "rst")
		b.ReportAllocs()
		// 稳态：首轮 DEL 命中已建键，其后为"DEL 不存在键 + 闸门复位"的
		// 往返开销度量（Reset 幂等语义本身允许连发）。
		for range b.N {
			if err := impl.Reset(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func mkBenchCfImpl(rc *redisClient, key string, cfg cuckooConfig) cuckooFilterImpl {
	impl := &cfCmdImpl{client: rc, key: key, cfg: cfg}
	impl.once.Store(new(sync.Once)) // 构造纪律：atomic.Pointer 零值 Load 为 nil
	return impl
}

func mkBenchHashImpl(rc *redisClient, key string, cfg cuckooConfig) cuckooFilterImpl {
	return newHashImpl(rc, key, cfg)
}

// --- 路径 × 环境 四组合 ---

func BenchmarkCuckooCfStandalone(b *testing.B) {
	benchCuckooImpl(b, "REDIS_URL", "cf", mkBenchCfImpl)
}

func BenchmarkCuckooCfCluster(b *testing.B) {
	benchCuckooImpl(b, "REDIS_CLUSTER", "cf", mkBenchCfImpl)
}

func BenchmarkCuckooHashStandalone(b *testing.B) {
	benchCuckooImpl(b, "REDIS_URL", "hash", mkBenchHashImpl)
}

func BenchmarkCuckooHashCluster(b *testing.B) {
	benchCuckooImpl(b, "REDIS_CLUSTER", "hash", mkBenchHashImpl)
}
