package redis

// bloom 双实现 Benchmark 矩阵（第五阶段 任务 2）：
//
//	路径 × 环境：
//	  - bfCmdImpl（原生 BF.*，白盒 newBFImpl 直构，需 bf 模块环境）
//	  - bitmapImpl 单键（白盒 newBitmapImpl 直构，带模块环境也恒走回退路径）
//	  - bitmapImpl 分片 8（仅 ModeCluster 生效，cfg.shardCount 白盒设置）
//	  - （bfCmdImpl 分片 8 一并覆盖）
//	  全部**白盒直构**实现路径——工厂仅保留能力探测的 auto 分派，
//	  矩阵的路径对照由直构构造保证）。
//	操作：Add / Exists / AddMulti / ExistsMulti / Card / Info / Reset
//	批量 size：100 / 1000 / 10000（Multi 类与 Reset 前置写入）
//	环境：真单机（REDIS_URL）、真集群（REDIS_CLUSTER）、miniredis（仅补
//	  既有 BenchmarkBitmapAdd/Multi 没有的操作，size 缩至 1000——miniredis
//	  Lua 逐位解释执行，10000 批量墙钟过慢、数据无增量意义）。
//
// 纪律：缺 env 即 b.Skip；键 bench: 前缀 + 随机后缀，b.Cleanup 逐键删除
// （分片形态禁多 key 合成 DEL——集群 CROSSSLOT）；benchtime 一律由运行
// 命令行控制，代码不设；重准备阶段用 b.StopTimer 隔离计时。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis"
)

// benchKey 生成共享实例上的隔离 bench 键：bench: 前缀 + 随机 hex + 用途
// 后缀，收尾经 b.Cleanup 逐键 Del。
func benchKey(suffix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("bench:%s:%s", hex.EncodeToString(b), suffix)
}

// benchSeq 生成迭代间全局唯一的 item（避免键内重复造成的语义/成本漂移）。
type benchSeq struct {
	tag string
	n   atomic.Int64
}

func (s *benchSeq) next() any {
	return fmt.Sprintf("%s-%d", s.tag, s.n.Add(1))
}

func (s *benchSeq) batch(size int) []any {
	items := make([]any, size)
	for i := range items {
		items[i] = s.next()
	}
	return items
}

// benchStandaloneClient / benchClusterClient：真环境 bench 守卫（internal
// 不能 import redis/test 包，自建 Getenv 守卫）。bf 模块可用性由调用方
// 决定是否成为 skip 条件。
func benchStandaloneClient(b *testing.B) (*redisClient, bool) {
	b.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		b.Skip("REDIS_URL 未设置：跳过真单机 bench")
	}
	rdb, err := NewWithUrl(url)
	if err != nil {
		b.Fatalf("NewWithUrl: %v", err)
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		b.Fatalf("unexpected client type %T", rdb)
	}
	b.Cleanup(func() { _ = rc.GracefulClose(context.Background()) })
	return rc, rdb.Capability().HasModule("bf")
}

func benchClusterClient(b *testing.B) (*redisClient, bool) {
	b.Helper()
	raw := os.Getenv("REDIS_CLUSTER")
	if raw == "" {
		b.Skip("REDIS_CLUSTER 未设置：跳过真集群 bench")
	}
	rdb, err := NewWithUrl(raw)
	if err != nil {
		b.Fatalf("NewWithUrl: %v", err)
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		b.Fatalf("unexpected client type %T", rdb)
	}
	b.Cleanup(func() { _ = rc.GracefulClose(context.Background()) })
	return rc, rdb.Capability().HasModule("bf")
}

// benchCleanupKeys 按白盒 sharder 口径逐键删除全部物理键（分片形态禁
// 合成单条多 key DEL）。
func benchCleanupKeys(b *testing.B, rc *redisClient, f BloomFilter) {
	b.Helper()
	var keys []string
	switch impl := f.(type) {
	case *bfCmdImpl:
		keys = impl.sharder.allKeys()
	case *bitmapImpl:
		keys = impl.sharder.allKeys()
	default:
		b.Fatalf("unknown impl %T", f)
	}
	b.Cleanup(func() {
		ctx := context.Background()
		for _, k := range keys {
			_ = rc.Del(ctx, k).Err()
		}
	})
}

// benchNewFilter 按路径名白盒构造过滤器；容量统一 1M、FPR 0.01、
// FailOpen。shardCount 仅集群模式生效（resolveBloomSharding 契约）。
func benchNewFilter(rc *redisClient, path, key string) BloomFilter {
	cfg := defaultBloomConfig()
	cfg.capacity = 1_000_000
	cfg.falsePositive = 0.01
	cfg.policy = FailOpen
	switch path {
	case "bf":
		return rc.newBFImpl(key, cfg)
	case "bf_s8":
		cfg.shardCount = 8
		return rc.newBFImpl(key, cfg)
	case "bitmap":
		return newBitmapImpl(rc, key, cfg)
	case "bitmap_s8":
		cfg.shardCount = 8
		return newBitmapImpl(rc, key, cfg)
	default:
		panic("bench: unknown bloom path " + path)
	}
}

// benchPathMatrix 为一个（环境 × 路径）组合注册全部操作叶子。
// 每叶子独立物理键（benchKey 随机 base），互踩不可能发生。
func benchPathMatrix(b *testing.B, rc *redisClient, path string) {
	ctx := context.Background()
	mk := func(suffix string) BloomFilter {
		return benchNewFilter(rc, path, benchKey(path+"-"+suffix))
	}

	b.Run("Add", func(b *testing.B) {
		f := mk("add")
		benchCleanupKeys(b, rc, f)
		seq := &benchSeq{tag: benchKey("item")}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := f.Add(ctx, seq.next()); err != nil {
				b.Fatalf("Add: %v", err)
			}
		}
	})

	b.Run("Exists", func(b *testing.B) {
		f := mk("exists")
		benchCleanupKeys(b, rc, f)
		pool := (&benchSeq{tag: benchKey("pool")}).batch(1000)
		b.StopTimer()
		if _, err := f.AddMulti(ctx, pool...); err != nil {
			b.Fatalf("预灌入 AddMulti: %v", err)
		}
		b.StartTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := f.Exists(ctx, pool[i%len(pool)]); err != nil {
				b.Fatalf("Exists: %v", err)
			}
		}
	})

	for _, size := range []int{100, 1000, 10000} {
		size := size
		b.Run(fmt.Sprintf("AddMulti/%d", size), func(b *testing.B) {
			f := mk(fmt.Sprintf("am%d", size))
			benchCleanupKeys(b, rc, f)
			seq := &benchSeq{tag: benchKey("am")}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := f.AddMulti(ctx, seq.batch(size)...); err != nil {
					b.Fatalf("AddMulti(%d): %v", size, err)
				}
			}
		})

		b.Run(fmt.Sprintf("ExistsMulti/%d", size), func(b *testing.B) {
			f := mk(fmt.Sprintf("em%d", size))
			benchCleanupKeys(b, rc, f)
			pool := (&benchSeq{tag: benchKey("em")}).batch(size)
			b.StopTimer()
			if _, err := f.AddMulti(ctx, pool...); err != nil {
				b.Fatalf("预灌入 AddMulti: %v", err)
			}
			b.StartTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := f.ExistsMulti(ctx, pool...); err != nil {
					b.Fatalf("ExistsMulti(%d): %v", size, err)
				}
			}
		})

		b.Run(fmt.Sprintf("Reset/%d", size), func(b *testing.B) {
			f := mk(fmt.Sprintf("rst%d", size))
			benchCleanupKeys(b, rc, f)
			seq := &benchSeq{tag: benchKey("rst")}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// 前置写入不计入计时：每迭代重灌 size 个唯一 item，
				// 度量"满负荷键 → Reset"的真实清空成本
				b.StopTimer()
				if _, err := f.AddMulti(ctx, seq.batch(size)...); err != nil {
					b.Fatalf("Reset 前置 AddMulti(%d): %v", size, err)
				}
				b.StartTimer()
				if err := f.Reset(ctx); err != nil {
					b.Fatalf("Reset: %v", err)
				}
			}
		})
	}

	b.Run("Card", func(b *testing.B) {
		f := mk("card")
		benchCleanupKeys(b, rc, f)
		b.StopTimer()
		if _, err := f.AddMulti(ctx, (&benchSeq{tag: benchKey("card")}).batch(1000)...); err != nil {
			b.Fatalf("预灌入 AddMulti: %v", err)
		}
		b.StartTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := f.Card(ctx); err != nil {
				b.Fatalf("Card: %v", err)
			}
		}
	})

	b.Run("Info", func(b *testing.B) {
		f := mk("info")
		benchCleanupKeys(b, rc, f)
		b.StopTimer()
		if _, err := f.AddMulti(ctx, (&benchSeq{tag: benchKey("info")}).batch(1000)...); err != nil {
			b.Fatalf("预灌入 AddMulti: %v", err)
		}
		b.StartTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := f.Info(ctx); err != nil {
				b.Fatalf("Info: %v", err)
			}
		}
	})
}

// BenchmarkBloomStandalone 真单机矩阵：bfCmdImpl（需 bf 模块，缺则跳过
// 该路径）+ bitmapImpl 单键。
func BenchmarkBloomStandalone(b *testing.B) {
	rc, hasBF := benchStandaloneClient(b)
	if hasBF {
		benchPathMatrix(b, rc, "bf")
	} else {
		b.Log("服务器无 bf 模块：跳过 bf 路径矩阵")
	}
	benchPathMatrix(b, rc, "bitmap")
}

// BenchmarkBloomCluster 真集群矩阵：单键与分片 8（WithShardCount 语义的
// 白盒等价 cfg.shardCount=8）× bf/bitmap 四组合。
func BenchmarkBloomCluster(b *testing.B) {
	rc, hasBF := benchClusterClient(b)
	if rc.Mode() != ModeCluster {
		b.Fatalf("REDIS_CLUSTER 连接模式应为 ModeCluster，got %v", rc.Mode())
	}
	if hasBF {
		benchPathMatrix(b, rc, "bf")
		benchPathMatrix(b, rc, "bf_s8")
	} else {
		b.Log("集群无 bf 模块：跳过 bf 路径矩阵")
	}
	benchPathMatrix(b, rc, "bitmap")
	benchPathMatrix(b, rc, "bitmap_s8")
}

// BenchmarkBloomMini miniredis 补既有 BenchmarkBitmapAdd/Multi 没有的
// bitmap 操作（Exists / ExistsMulti / Card / Info / Reset；AddMulti 既有
// 64 批基线保留不动）。size 缩至 {100, 1000}：miniredis Lua 逐位解释
// 执行，10000 批墙钟过慢且无增量信息。
func BenchmarkBloomMini(b *testing.B) {
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(mr.Close)
	rdb := New(WithAddr(mr.Addr()))
	b.Cleanup(func() { _ = rdb.GracefulClose(context.Background()) })
	rc, ok := rdb.(*redisClient)
	if !ok {
		b.Fatalf("unexpected client type %T", rdb)
	}

	ctx := context.Background()
	newF := func() *bitmapImpl {
		cfg := defaultBloomConfig()
		cfg.capacity = 1_000_000
		cfg.falsePositive = 0.01
		cfg.policy = FailOpen
		return newBitmapImpl(rc, "bench:mini:bloom", cfg)
	}

	b.Run("Exists", func(b *testing.B) {
		f := newF()
		pool := (&benchSeq{tag: "bmn-exist"}).batch(1000)
		b.StopTimer()
		if _, err := f.AddMulti(ctx, pool...); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		for i := 0; i < b.N; i++ {
			if _, err := f.Exists(ctx, pool[i%len(pool)]); err != nil {
				b.Fatal(err)
			}
		}
	})

	for _, size := range []int{100, 1000} {
		size := size
		b.Run(fmt.Sprintf("ExistsMulti/%d", size), func(b *testing.B) {
			f := newF()
			pool := (&benchSeq{tag: "bmn-em"}).batch(size)
			b.StopTimer()
			if _, err := f.AddMulti(ctx, pool...); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if _, err := f.ExistsMulti(ctx, pool...); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("Reset/%d", size), func(b *testing.B) {
			f := newF()
			seq := &benchSeq{tag: "bmn-rst"}
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				if _, err := f.AddMulti(ctx, seq.batch(size)...); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := f.Reset(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}

	b.Run("Card", func(b *testing.B) {
		f := newF()
		b.StopTimer()
		if _, err := f.AddMulti(ctx, (&benchSeq{tag: "bmn-card"}).batch(1000)...); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		for i := 0; i < b.N; i++ {
			if _, err := f.Card(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Info", func(b *testing.B) {
		f := newF()
		b.StopTimer()
		if _, err := f.AddMulti(ctx, (&benchSeq{tag: "bmn-info"}).batch(1000)...); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		for i := 0; i < b.N; i++ {
			if _, err := f.Info(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}
