package redis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
	goredis "github.com/redis/go-redis/v9"
	"github.com/zeebo/xxh3"
)

// bloomTestKey 生成共享 Redis 实例上的隔离测试 key：统一 "bloomtest:" 前缀 +
// 随机 hex 段，避免与他人 key 冲突；用例收尾必须 Del 清理。
func bloomTestKey(suffix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("bloomtest:%s:%s", hex.EncodeToString(b), suffix)
}

// anyItems 把 []string 转为 []any，供 item any 化后的批量接口调用
// （测试内部辅助；生产路径的编码校验在 marshalItem/group 内完成）。
func anyItems(items []string) []any {
	args := make([]any, len(items))
	for i, v := range items {
		args[i] = v
	}
	return args
}

// routeBytes 返回 item 的路由字节——item any 化后 indexOf/keyFor/group 的
// 入参口径（string 与 marshalItem(string) 同字节，测试里直取）。
func routeBytes(item string) []byte { return []byte(item) }

// newSimBitmap 构造仅用于哈希纯计算的 bitmapImpl（client 为 nil，
// 只调用 hashs/m/k 等不触网的方法，用于位图算法层的统计回归）。
func newSimBitmap(t *testing.T, n int64, p float64) *bitmapImpl {
	t.Helper()
	cfg := defaultBloomConfig()
	cfg.capacity = n
	cfg.falsePositive = p
	return newBitmapImpl(nil, "sim", cfg)
}

// TestBitmapHashParity 验证 C2 的奇偶不变量与双哈希结构：
//   - m 恒为奇数（newBitmapImpl 中 m|=1）；
//   - 步长 h2 恒为奇数（hashs 中 Lo|1），且实现确实使用该步长
//     （通过 p_{i+1} == (p_i + h2) mod m 反推一致性）；
//   - k 个位置互不相同；
//   - 抽样统计置位位置无显著奇偶偏置（修复前 m/h2 同为偶数时，
//     轨道减半会使探测位被困在单一奇偶半值域）。
func TestBitmapHashParity(t *testing.T) {
	cases := []struct {
		n int64
		p float64
	}{
		{100_000, 0.01},
		{100_000, 0.001},
		{1_000_000, 0.01},
	}

	for _, c := range cases {
		t.Run(fmt.Sprintf("n=%d_p=%v", c.n, c.p), func(t *testing.T) {
			b := newSimBitmap(t, c.n, c.p)

			// m 恒奇
			if b.m%2 != 1 {
				t.Fatalf("m 必须为奇数（m|=1 奇化），got %d", b.m)
			}
			// k 基于奇化后的 m：k = ceil(ln2·m/n)，验证公式一致性
			if want := bloomHashCount(c.n, b.m); b.k != want {
				t.Fatalf("k 必须基于奇化后的 m 计算：got %d want %d", b.k, want)
			}

			var odd, total, dupItems int
			for i := range 5000 {
				item := fmt.Sprintf("parity-%d", i)
				sum := xxh3.Hash128([]byte(item))
				h1, h2 := sum.Hi, sum.Lo|1
				if h2%2 != 1 {
					t.Fatalf("步长 h2 必须恒奇，item=%s h2=%d", item, h2)
				}

				pos, err := b.hashs(item)
				if err != nil {
					t.Fatalf("hashs(%s)：%v", item, err)
				}
				if len(pos) != int(b.k) {
					t.Fatalf("位置数 got %d want %d", len(pos), b.k)
				}

				seen := make(map[uint64]bool, len(pos))
				for _, x := range pos {
					if x >= b.m {
						t.Fatalf("位置越界：got %d m=%d", x, b.m)
					}
					seen[x] = true
					if x%2 == 1 {
						odd++
					}
					total++
				}
				if len(seen) < len(pos) {
					dupItems++
				}

				// 反推实现使用的 h1/h2（含 uint64 自然回绕，与 hashs 语义一致）：
				// positions[i] = (h1 + uint64(i)*h2) % m
				for j := range pos {
					if want := (h1 + uint64(j)*h2) % b.m; pos[j] != want {
						t.Fatalf("哈希结构一致性失败：item=%s pos[%d] got %d want %d", item, j, pos[j], want)
					}
				}
			}
			if dupItems > 0 {
				t.Logf("注意：k 个位置存在重复的 item 数 = %d/5000（奇公因子导致的轨道收缩，见 hashs 注释的诚实声明）", dupItems)
			}

			// 抽样奇偶偏置：期望接近 1/2，宽松区间 0.45~0.55
			ratio := float64(odd) / float64(total)
			if ratio < 0.45 || ratio > 0.55 {
				t.Fatalf("位置奇偶分布存在偏置：odd ratio = %.4f（期望 ~0.5，容差 0.45~0.55）", ratio)
			}
		})
	}
}

// TestBitmapFPRRegression 是 C2 的统计回归：用纯 Go []bool 位图模拟
// bitmapImpl 的哈希布局（不触 Redis），灌入 n 个 item 后以 n 个未灌入
// item 测误判率，断言 FPR ≤ 1.2×p。
//
// 旧实现（FNV-1a 双哈希，m 与 h2 均为偶数）在 p=0.001 档 FPR 实测超标
// 34.95 倍（轨道减半缺陷）；xxh3-128 + m|1 + h2|1 修复后应落回理论值附近。
// item 序列完全确定（无随机源），失败可稳定复现。
//
// 注：规格原要求"断言任意 item 的 k 个位置互不相同"，该绝对断言与 C2
// 声明的"仅消除公因子 2、非完整互质证明"数学上不可共存（m 与 h2 的奇
// 公因子必然造成轨道收缩与重复位），故放宽为统计断言：重复位 item 占比
// < 0.5%（实测 ppm 级），功能正确性以 FPR ≤ 1.2×p 为主断言。
func TestBitmapFPRRegression(t *testing.T) {
	cases := []struct {
		n int64
		p float64
	}{
		{100_000, 0.01},
		{100_000, 0.001},
		{1_000_000, 0.01},
		{1_000_000, 0.001},
	}

	for _, c := range cases {
		t.Run(fmt.Sprintf("n=%d_p=%v", c.n, c.p), func(t *testing.T) {
			b := newSimBitmap(t, c.n, c.p)
			bitset := make([]bool, b.m)

			for i := int64(0); i < c.n; i++ {
				positions, err := b.hashs(fmt.Sprintf("inserted-%d", i))
				if err != nil {
					t.Fatalf("hashs inserted-%d：%v", i, err)
				}
				for _, pos := range positions {
					bitset[pos] = true
				}
			}

			var fp int64
			var dupItems int64
			for i := int64(0); i < c.n; i++ {
				positions, err := b.hashs(fmt.Sprintf("missing-%d", i))
				if err != nil {
					t.Fatalf("hashs missing-%d：%v", i, err)
				}
				// 位置互异性（规格断言的统计化放宽，偏差说明见函数注释）：
				// 存在重复位置的 item 占比必须极低（<0.5%）。绝对互异需 h2 与
				// m 完整互质，而 C2 仅消除公因子 2（奇公因子 3/5/… 造成 ppm 级
				// 轨道收缩，属 hashs 注释已声明的残余缺陷）。
				seen := make(map[uint64]struct{}, len(positions))
				for _, pos := range positions {
					seen[pos] = struct{}{}
				}
				if len(seen) < len(positions) {
					dupItems++
				}

				hit := true
				for _, pos := range positions {
					if !bitset[pos] {
						hit = false
						break
					}
				}
				if hit {
					fp++
				}
			}

			got := float64(fp) / float64(c.n)
			if got > 1.2*c.p {
				t.Fatalf("FPR 超标：got %.6f > 1.2×p=%.6f（n=%d p=%v m=%d k=%d）", got, 1.2*c.p, c.n, c.p, b.m, b.k)
			}
			dupRatio := float64(dupItems) / float64(c.n)
			if dupRatio >= 0.005 {
				t.Fatalf("位置重复率异常：%.4f（n=%d m=%d k=%d，远超奇公因子轨道收缩的 ppm 级预期，疑哈希退化）", dupRatio, c.n, b.m, b.k)
			}
			t.Logf("FPR = %.6f（阈值 %.6f，n=%d m=%d k=%d），位置重复 item 占比 = %.6f", got, 1.2*c.p, c.n, b.m, b.k, dupRatio)
		})
	}
}

// TestBloomOptionBounds 验证 C1 参数校验：非法值静默回落默认、不 panic；
// 合法值正常采纳。
func TestBloomOptionBounds(t *testing.T) {
	t.Run("非法值回落默认", func(t *testing.T) {
		cfg := defaultBloomConfig()
		WithCapacity(0)(&cfg)
		WithCapacity(-5)(&cfg)
		if cfg.capacity != 1_000_000 {
			t.Fatalf("WithCapacity(0)/(-5) 应保留默认 1e6，got %d", cfg.capacity)
		}
		WithFalsePositive(0)(&cfg)
		WithFalsePositive(-1)(&cfg)
		WithFalsePositive(1)(&cfg)
		if cfg.falsePositive != 0.01 {
			t.Fatalf("WithFalsePositive 非法值应保留默认 0.01，got %v", cfg.falsePositive)
		}
		// 回落默认后构造不 panic
		_ = newBitmapImpl(nil, "k", cfg)
	})

	t.Run("合法值采纳", func(t *testing.T) {
		cfg := defaultBloomConfig()
		WithCapacity(5000)(&cfg)
		WithFalsePositive(0.001)(&cfg)
		if cfg.capacity != 5000 || cfg.falsePositive != 0.001 {
			t.Fatalf("合法值未采纳：capacity=%d fp=%v", cfg.capacity, cfg.falsePositive)
		}
	})
}

// TestNewBitmapImplPanic 验证 C1 的位图上限 fail-fast：m 超过 Redis
// 位图上限 2^32-1 bit 时 panic（先例：MustConstraint）。
func TestNewBitmapImplPanic(t *testing.T) {
	cases := []struct {
		name      string
		capacity  int64
		p         float64
		wantPanic bool
	}{
		{"上限内最大档 1 亿@1%", 100_000_000, 0.01, false},
		{"超限 4.5 亿@1%", 450_000_000, 0.01, true},
		{"int64 极端值（bloomBitCount 饱和路径）", math.MaxInt64, 0.001, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if c.wantPanic {
					if r == nil {
						t.Fatalf("应 panic（超 2^32-1 位图上限），实际未 panic")
					}
					msg := fmt.Sprint(r)
					if !strings.Contains(msg, "2^32-1") {
						t.Fatalf("panic 消息应含位图上限说明，got %q", msg)
					}
				} else if r != nil {
					t.Fatalf("不应 panic，got %v", r)
				}
			}()
			cfg := defaultBloomConfig()
			cfg.capacity = c.capacity
			cfg.falsePositive = c.p
			_ = newBitmapImpl(nil, "k", cfg)
		})
	}
}

// TestEstimateNumItems 验证 C4 估计量的纯函数行为：0 位置 → 0；
// 标准公式中间值；bitsSet 饱和（>=m）钳制到容量上界防 NaN/Inf。
func TestEstimateNumItems(t *testing.T) {
	b := newSimBitmap(t, 10_000, 0.01) // m=95851, k=7（奇化后）

	if got := b.estimateNumItems(0); got != 0 {
		t.Fatalf("bitsSet=0 应估 0，got %d", got)
	}

	// 公式验证：numItems = -(m/k)·ln(1-bitsSet/m)
	bitsSet := int64(b.m / 2)
	want := int64(math.Round(-(float64(b.m) / float64(b.k)) * math.Log(1-float64(bitsSet)/float64(b.m))))
	if got := b.estimateNumItems(bitsSet); got != want {
		t.Fatalf("公式估计失败：got %d want %d", got, want)
	}

	// 饱和钳制：bitsSet >= m 时 fraction <= 0 → 钳到 capacity（非 NaN/Inf/负数）
	if got := b.estimateNumItems(int64(b.m)); got != b.capacity {
		t.Fatalf("饱和时应钳到容量上界 %d，got %d", b.capacity, got)
	}
	if got := b.estimateNumItems(int64(b.m) + 100); got != b.capacity {
		t.Fatalf("超饱和时应钳到容量上界 %d，got %d", b.capacity, got)
	}
}

// TestBitmapConcurrentAddRealRedis 验证 bitmap 路径 Lua 脚本的并发原子性：
// 200 个 goroutine 并发 Add 同一 item，必须恰有 1 个返回"新增"且全部无错误。
//
// 必须在真实 Redis 上跑：miniredis 的 EVAL 非原子（逐条执行、可被并发插入
// 交错），该断言在 miniredis 上不成立。
// 本用例在 package redis 内部构造 bitmapImpl——部署实例加载了 bf 模块时
// NewBloomFilter 工厂会分派到 bfCmdImpl，无法经公开 API 得到 bitmapImpl。
//
// 需要 REDIS_URL（如 redis://:pass@host:6379），未设置时跳过。
// key 带 bloomtest:<随机> 唯一前缀，收尾 Del 清理（共享实例，严禁 FLUSHDB）。
func TestBitmapConcurrentAddRealRedis(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skip real-Redis concurrency test")
	}

	rdb, err := NewWithUrl(url)
	if err != nil {
		t.Fatalf("NewWithUrl: %v", err)
	}
	defer func() { _ = rdb.GracefulClose(context.Background()) }()

	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}

	ctx := context.Background()
	key := bloomTestKey("conc")
	if err := rc.Del(ctx, key).Err(); err != nil {
		t.Fatalf("prepare key: %v", err)
	}
	defer func() { _ = rc.Del(ctx, key).Err() }()

	cfg := defaultBloomConfig()
	cfg.policy = FailOpen
	b := newBitmapImpl(rc, key, cfg)

	const n = 200
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		added    int
		notAdded int
		errs     []error
	)
	start := make(chan struct{})

	for range n {
		wg.Go(func() {
			<-start // 并发闸门：所有 goroutine 尽可能同时发起
			v, err := b.Add(ctx, "race-item")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, err)
			case v:
				added++
			default:
				notAdded++
			}
		})
	}
	close(start)
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("err 必须全为 nil，got %d 个（首个：%v）", len(errs), errs[0])
	}
	if added != 1 {
		t.Fatalf("原子性断言失败：200 并发 Add 同一 item 应恰 1 个 true，got %d（Lua 脚本非原子或位图被污染）", added)
	}
	if notAdded != n-1 {
		t.Fatalf("其余应全为 false：got %d want %d", notAdded, n-1)
	}
}

// --- C3a：Lua 能力记忆（三态分诊 + 状态迁移） ---

// TestClassifyLuaError 验证 EVAL 失败分诊纯函数：仅"命令不存在/被禁"类判为
// Unsupported（允许置记忆 -1）；瞬态错误判 Unavailable（不动记忆）；
// WRONGTYPE/NOPERM 等数据类判 DataError（原样透传，不得污染记忆——误置位会
// 把好服务器永久打入慢路径）。ctx 取消/超时是调用方行为而非服务器能力问题，
// 归入 DataError 原样透传。
func TestClassifyLuaError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want luaVerdict
	}{
		{"ERR unknown command（代理屏蔽 EVAL）", errors.New("ERR unknown command 'EVAL'"), luaVerdictUnsupported},
		{"unknown command（小写变体）", errors.New("unknown command eval"), luaVerdictUnsupported},
		{"ERR unknown（其他未知类）", errors.New("ERR unknown subcommand"), luaVerdictUnsupported},
		{"not allowed（ACL/脚本禁用）", errors.New("The 'EVAL' command is not allowed from scripts"), luaVerdictUnsupported},
		{"WRONGTYPE 数据类", errors.New("WRONGTYPE Operation against a key holding the wrong kind of value"), luaVerdictDataError},
		{"NOPERM 数据类", errors.New("NOPERM this user has no permissions to run the 'eval' command"), luaVerdictDataError},
		{"dial 失败瞬态", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, luaVerdictUnavailable},
		{"EOF 瞬态", io.EOF, luaVerdictUnavailable},
		{"连接池超时瞬态", errors.New("redis: connection pool timeout"), luaVerdictUnavailable},
		{"ctx 取消透传", context.Canceled, luaVerdictDataError},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyLuaError(c.err); got != c.want {
				t.Errorf("classifyLuaError(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// newMiniRedisClient 在本地 miniredis 上构造 client，返回内部 *redisClient
// （内部测试直达非导出成员）与 miniredis 实例（独享内存实例，key 无需
// bloomtest 隔离，可用 mr.Get 直接比对位图内容）。
func newMiniRedisClient(t *testing.T) (*redisClient, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)

	rdb := New(WithAddr(mr.Addr()))
	t.Cleanup(func() { _ = rdb.GracefulClose(context.Background()) })

	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}
	return rc, mr
}

// TestLuaSupportStateTransition 端到端验证 luaSupport 三态迁移规则：
//   - 初始未知 0；EVAL 成功 → 置 1；
//   - 数据类错误（WRONGTYPE）→ 记忆不动（保持未知 0）且错误原样透传；
//   - 瞬态错误（服务器关闭）→ 记忆保持 1 不倒退，走 fallbackBool 兜底；
//   - 记忆为 -1 → 入口跳过 EVAL，由 addFallback（pipeline 兜底）完成操作，
//     记忆保持 -1（慢路径不得"复活"记忆）。
func TestLuaSupportStateTransition(t *testing.T) {
	ctx := context.Background()

	t.Run("成功后记忆置 1", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		if got := rc.luaState(); got != 0 {
			t.Fatalf("初始应为未知 0，got %d", got)
		}
		b := newBitmapImpl(rc, "st:ok", defaultBloomConfig())
		added, err := b.add(ctx, "x")
		if err != nil || !added {
			t.Fatalf("add 失败：added=%v err=%v", added, err)
		}
		if got := rc.luaState(); got != 1 {
			t.Fatalf("EVAL 成功后记忆应置 1，got %d", got)
		}
	})

	t.Run("数据类错误透传且记忆不动", func(t *testing.T) {
		rc, mr := newMiniRedisClient(t)
		// 目标 key 为 hash 类型：EVAL 内 GETBIT 触发 WRONGTYPE（数据类）
		mr.HSet("st:wt", "field", "v")
		b := newBitmapImpl(rc, "st:wt", defaultBloomConfig())

		_, err := b.add(ctx, "x")
		if err == nil {
			t.Fatal("WRONGTYPE 应原样透传错误，got nil")
		}
		if got := rc.luaState(); got != 0 {
			t.Fatalf("数据类错误不得改动记忆（应保持未知 0，更不得置 -1），got %d", got)
		}
	})

	t.Run("瞬态错误记忆不倒退且走兜底", func(t *testing.T) {
		rc, mr := newMiniRedisClient(t)
		cfg := defaultBloomConfig()
		cfg.policy = FailOpen // 工厂 NewBloomFilter 的默认；直接构造需显式设
		b := newBitmapImpl(rc, "st:down", cfg)
		if _, err := b.add(ctx, "x"); err != nil {
			t.Fatal(err) // 先建立记忆 1
		}
		if got := rc.luaState(); got != 1 {
			t.Fatalf("前置条件：记忆应为 1，got %d", got)
		}

		mr.Close() // 模拟服务器宕机（Cleanup 重复 Close 幂等安全）

		// 默认策略 FailOpen：兜底返回 true + ErrRedisUnavailable
		got, err := b.add(ctx, "y")
		if !errors.Is(err, ErrRedisUnavailable) {
			t.Fatalf("瞬态错误应走 fallbackBool 兜底（ErrRedisUnavailable），got %v", err)
		}
		if !got {
			t.Fatalf("FailOpen 兜底值应为 true，got %v", got)
		}
		if state := rc.luaState(); state != 1 {
			t.Fatalf("瞬态错误不得改动记忆（应仍为 1），got %d", state)
		}
	})

	t.Run("已知不支持时跳过 EVAL 走兜底", func(t *testing.T) {
		rc, mr := newMiniRedisClient(t)
		rc.luaMarkUnsupported()

		b := newBitmapImpl(rc, "st:nolua", defaultBloomConfig())
		added, err := b.add(ctx, "z")
		if err != nil || !added {
			t.Fatalf("兜底路径 add 失败：added=%v err=%v", added, err)
		}
		ok, err := b.exists(ctx, "z")
		if err != nil || !ok {
			t.Fatalf("兜底路径 exists 失败：ok=%v err=%v", ok, err)
		}
		if _, err := mr.Get("st:nolua"); err != nil {
			t.Fatalf("pipeline 兜底应已写入位图，mr.Get 失败：%v", err)
		}
		if got := rc.luaState(); got != -1 {
			t.Fatalf("慢路径执行后记忆应保持 -1，got %d", got)
		}
	})
}

// TestBitmapFallbackConsistency 验证兜底路径与 Lua 路径的一致性：
// miniredis 上对同一批 item，直接调用 b.add（Lua 原子路径）与
// b.addFallback（pipeline 非原子兜底），两路径使用各自独立的 key——
// 判定基于"调用前"的位图快照，共用位图会互相污染使对照失真：
//   - 位图最终内容逐字节一致（同一 hashs 布局，验证 pipeline 写入正确）；
//   - Exists 两路径结果完全一致（存在性判定粒度相同）；
//   - Add 返回值逐个完全相等（硬断言）：判据修正后两路径同为"k 位至少
//     一位在调用前为 0 即新增"（Lua 脚本 GETBIT 先于 SETBIT；addFallback
//     单次 SETBIT pipeline、以返回的旧值判定）。历史上 Lua 路径误用
//     all_zero（k 位
//     全 0 才算新增），高填充率下系统性漏判，本断言即该缺陷的收敛防线，
//     任一侧语义回退都会立即失败。
func TestBitmapFallbackConsistency(t *testing.T) {
	rc, mr := newMiniRedisClient(t)
	ctx := context.Background()

	cfg := defaultBloomConfig()
	cfg.capacity = 10000
	bLua := newBitmapImpl(rc, "cons:lua", cfg)
	bFb := newBitmapImpl(rc, "cons:fb", cfg)

	const n = 200
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf("cons-item-%d", i)
	}

	for i, item := range items {
		a1, err := bLua.add(ctx, item)
		if err != nil {
			t.Fatalf("Lua 路径 add(%s)：%v", item, err)
		}
		a2, err := bFb.addFallback(ctx, item)
		if err != nil {
			t.Fatalf("兜底路径 addFallback(%s)：%v", item, err)
		}
		// 两路径逐个完全相等（判据统一为"调用前至少一位为 0"）。
		if a1 != a2 {
			t.Fatalf("add(%s) 第 %d 个 item 两路径返回值不一致：Lua=%v 兜底=%v（语义再次分叉）", item, i, a1, a2)
		}
	}

	// 命中 + 未命中混合的 exists 一致性
	checks := make([]string, 0, n/7+2)
	for i := 0; i < n; i += 7 {
		checks = append(checks, items[i])
	}
	checks = append(checks, "cons-missing-1", "cons-missing-2")
	for _, item := range checks {
		e1, err := bLua.exists(ctx, item)
		if err != nil {
			t.Fatalf("Lua 路径 exists(%s)：%v", item, err)
		}
		e2, err := bFb.existsFallback(ctx, item)
		if err != nil {
			t.Fatalf("兜底路径 existsFallback(%s)：%v", item, err)
		}
		if e1 != e2 {
			t.Fatalf("exists(%s) 两路径不一致：%v vs %v", item, e1, e2)
		}
	}

	// 位图最终状态逐字节一致
	bmLua, err := mr.Get("cons:lua")
	if err != nil {
		t.Fatal(err)
	}
	bmFb, err := mr.Get("cons:fb")
	if err != nil {
		t.Fatal(err)
	}
	if bmLua != bmFb {
		t.Fatalf("两路径位图最终状态不一致：len %d vs %d", len(bmLua), len(bmFb))
	}
}

// TestBitmapAddSemanticsHighFill 是针对 bitmap Add 判据缺陷（all_zero →
// "调用前至少一位为 0"）的纯内存回归：n=20000、p=0.001（m=287553、k=10，
// 奇化后），灌满 2 万个全新元素后位图填充率约 50%——旧 all_zero 口径在此
// 密度下仅约 14.3% 返回 true（漏判 ~85%），真实例 BF.ADD 为 100%。
// 用 Go []bool 复刻新判据（先在置位前的快照上判"是否全 1"，再补缺位），
// 不依赖 Redis，item 序列完全确定、可稳定复现。
func TestBitmapAddSemanticsHighFill(t *testing.T) {
	const (
		n = 20_000
		p = 0.001
	)
	b := newSimBitmap(t, n, p)
	t.Logf("m=%d k=%d", b.m, b.k)

	bitset := make([]bool, b.m)

	// add 复刻 Lua 脚本新判据：k 位中至少一位在置位前为 0 → 新增（true），
	// 并把所有为 0 的位补置 1；k 位全 1 → false（可能存在，无缺位可补）。
	add := func(item string) bool {
		positions, err := b.hashs(item)
		if err != nil {
			t.Fatalf("hashs(%s)：%v", item, err)
		}
		added := false
		for _, pos := range positions {
			if !bitset[pos] {
				added = true // 判定基于置位之前的快照
			}
		}
		for _, pos := range positions {
			bitset[pos] = true
		}
		return added
	}

	var newTrue int
	for i := range n {
		if add(fmt.Sprintf("hf-inserted-%d", i)) {
			newTrue++
		}
	}
	// 全新元素绝大多数必须返回 true：仅当某 item 的 k 位恰好全被此前插入
	// 置 1 时才判"可能存在"（理论占比 ≈(1-e^(-k²n'/m))^k，p=0.001 下实测
	// ~0.02%）。旧 all_zero 口径此项仅 ~14.3%，断言 ≥99.9% 直接红。
	if ratio := float64(newTrue) / float64(n); ratio < 0.999 {
		t.Fatalf("全新元素 Add 返回 true 占比过低：%.4f（< 0.999，疑 all_zero 判据回归）", ratio)
	}

	// 灌完后重复 Add：k 位已全部置 1，必须 0% 返回 true（零容忍，逐个断言）。
	for i := range n {
		if add(fmt.Sprintf("hf-inserted-%d", i)) {
			t.Fatalf("重复元素 hf-inserted-%d 返回 true（k 位已全部置位，应恒为 false）", i)
		}
	}
	t.Logf("全新元素 true 占比 = %.4f（阈值 0.999），重复元素 true 数 = 0", float64(newTrue)/float64(n))
}

// TestBitmapAddDenseViaLua 验证 Lua 真路径（miniredis）在高密度位图上的
// 判据：预置 SetBit 构造"目标 item 的 k 位仅一位为 0"，Add 必须返回 true
// （旧 all_zero 实现在此场景必红）；补齐缺位后重复 Add 返回 false。
// 批量脚本（bitmapAddMultiScript）与单条脚本同构修改，同场景一并覆盖。
func TestBitmapAddDenseViaLua(t *testing.T) {
	rc, _ := newMiniRedisClient(t)
	ctx := context.Background()

	cfg := defaultBloomConfig()
	cfg.capacity = 10000
	b := newBitmapImpl(rc, "dense:lua", cfg)

	// setDenseSingleZero 将 item 的 k 位全部置 1 后留一位为 0：
	// 调用前"至少一位为 0"→ BF.ADD 语义下属新增。
	setDenseSingleZero := func(item string) {
		t.Helper()
		// 非分片实例：物理键即 sharder.base（key 字段已删除，等价表达）
		positions, err := b.hashs(item)
		if err != nil {
			t.Fatalf("hashs(%s)：%v", item, err)
		}
		for _, pos := range positions {
			if err := rc.SetBit(ctx, b.sharder.base, int64(pos), 1).Err(); err != nil {
				t.Fatalf("预置 SetBit(%d,1)：%v", pos, err)
			}
		}
		if err := rc.SetBit(ctx, b.sharder.base, int64(positions[0]), 0).Err(); err != nil {
			t.Fatalf("预置缺位 SetBit(%d,0)：%v", positions[0], err)
		}
	}

	const item = "dense-item"
	setDenseSingleZero(item)

	added, err := b.add(ctx, item)
	if err != nil {
		t.Fatalf("单条 Lua add：%v", err)
	}
	if !added {
		t.Fatalf("k 位仅一位为 0 的 Add 应返回 true（对齐 BF.ADD），got false——all_zero 判据回归")
	}
	// 缺位已补齐：k 位全 1，重复 Add 必须 false，且 Exists 为 true。
	if again, err := b.add(ctx, item); err != nil || again {
		t.Fatalf("补齐缺位后重复 Add 应返回 false，got %v（err=%v）", again, err)
	}
	if ok, err := b.exists(ctx, item); err != nil || !ok {
		t.Fatalf("补齐后 Exists 应为 true，got %v（err=%v）", ok, err)
	}

	// 批量脚本同构判据：另一 item 的 k 位仅一位为 0，AddMulti 应返回 [true]。
	const item2 = "dense-item-2"
	setDenseSingleZero(item2)

	vals, err := b.AddMulti(ctx, item2, item) // item2 缺位→true；item 全 1→false
	if err != nil {
		t.Fatalf("批量 Lua AddMulti：%v", err)
	}
	if len(vals) != 2 || vals[0] != true || vals[1] != false {
		t.Fatalf("AddMulti 高密度判据错误：got %v，期望 [true false]", vals)
	}
}

// TestBitmapMultiPositionsArgs 验证批量脚本参数布局：ARGV[0]=k，
// 随后按 items 入参顺序逐项展开 k 个位置（总长 1+n·k）——批量脚本的
// 顺序对应性由该布局保证。
func TestBitmapMultiPositionsArgs(t *testing.T) {
	b := newSimBitmap(t, 10000, 0.01)
	items := []any{"m1", "m2", "m3"}

	args, err := b.multiPositionsArgs(items)
	if err != nil {
		t.Fatalf("multiPositionsArgs：%v", err)
	}
	if len(args) != 1+len(items)*int(b.k) {
		t.Fatalf("参数总数 got %d want %d", len(args), 1+len(items)*int(b.k))
	}
	if kk, ok := args[0].(uint); !ok || kk != b.k {
		t.Fatalf("ARGV[0] 应为 k=%d，got %v", b.k, args[0])
	}
	for i, item := range items {
		want, err := b.hashs(item)
		if err != nil {
			t.Fatalf("hashs(%v)：%v", item, err)
		}
		for j, w := range want {
			if got := args[1+i*int(b.k)+j].(uint64); got != w {
				t.Fatalf("item %s 第 %d 位 got %d want %d（顺序对应性破坏）", item, j, got, w)
			}
		}
	}
}

// TestBitmapClusterFallbackRouting 在真实 Redis Cluster 上验证 bitmap 路径
// 两类执行形态对单 key 的 cluster 路由合法性：
//  1. 正常路径：单条 Lua 脚本（EVAL 单 KEYS[1]）+ 批量 Lua 脚本；
//  2. 兜底路径：强制 luaSupport=-1 后，pipeline（k 个 GETBIT/SETBIT 合并）
//     与批量逐条降级对同一 key 的所有命令落同一 slot，须正常执行。
//
// 共享实例纪律：key 带 bloomtest:<随机> 唯一前缀、收尾 Del；严禁 FLUSHDB。
// REDIS_CLUSTER 未设置时跳过（内部测试不能 import test 包——依赖图成环，
// 此处自建守卫）。
func TestBitmapClusterFallbackRouting(t *testing.T) {
	raw := os.Getenv("REDIS_CLUSTER")
	if raw == "" {
		t.Skip("REDIS_CLUSTER not set; skip cluster test")
	}
	rdb, err := NewWithUrl(raw)
	if err != nil {
		t.Fatalf("REDIS_CLUSTER URL 解析失败：%v", err)
	}
	t.Cleanup(func() { _ = rdb.GracefulClose(context.Background()) })

	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}
	ctx := context.Background()

	cfg := defaultBloomConfig()
	cfg.capacity = 10000

	t.Run("cluster Lua 单条与批量脚本", func(t *testing.T) {
		key := bloomTestKey("cl-lua")
		defer func() { _ = rc.Del(ctx, key).Err() }()

		b := newBitmapImpl(rc, key, cfg)
		if err := rc.Del(ctx, key).Err(); err != nil {
			t.Fatal(err)
		}

		added, err := b.add(ctx, "u1")
		if err != nil || !added {
			t.Fatalf("cluster Add 路由失败：added=%v err=%v", added, err)
		}
		multiArgs := []any{"u2", "u1", "u3"}
		addedMulti, err := b.AddMulti(ctx, multiArgs...)
		if err != nil {
			t.Fatalf("cluster AddMulti（批量 Lua）失败：%v", err)
		}
		// 顺序与入参严格对应：u2 新增、u1 已存在、u3 新增
		if len(addedMulti) != 3 || addedMulti[0] != true || addedMulti[1] != false || addedMulti[2] != true {
			t.Fatalf("AddMulti 顺序/语义错误：got %v", addedMulti)
		}
		exists, err := b.ExistsMulti(ctx, "u3", "nope", "u1")
		if err != nil {
			t.Fatalf("cluster ExistsMulti（批量 Lua）失败：%v", err)
		}
		if len(exists) != 3 || exists[0] != true || exists[1] != false || exists[2] != true {
			t.Fatalf("ExistsMulti 顺序错误：got %v", exists)
		}
	})

	t.Run("cluster pipeline 兜底路由", func(t *testing.T) {
		key := bloomTestKey("cl-fb")
		defer func() { _ = rc.Del(ctx, key).Err() }()

		rc.luaMarkUnsupported() // 入口跳过 EVAL → pipeline 兜底
		// 记忆为 client 级；本子测试排在 Lua 正常路径子测试之后，-1 不回溯影响，
		// client 由 Cleanup 关闭，无跨用例泄漏。

		b := newBitmapImpl(rc, key, cfg)
		added, err := b.add(ctx, "f1")
		if err != nil || !added {
			t.Fatalf("cluster pipeline 兜底 Add 失败：added=%v err=%v", added, err)
		}
		if ok, err := b.exists(ctx, "f1"); err != nil || !ok {
			t.Fatalf("cluster pipeline 兜底 Exists 失败：ok=%v err=%v", ok, err)
		}
		// AddMulti 在记忆 -1 时逐条降级（含 pipeline 兜底），结果顺序对应
		addedMulti, err := b.AddMulti(ctx, "f2", "f1", "f3")
		if err != nil {
			t.Fatalf("cluster 逐条降级 AddMulti 失败：%v", err)
		}
		if len(addedMulti) != 3 || addedMulti[0] != true || addedMulti[1] != false || addedMulti[2] != true {
			t.Fatalf("逐条降级 AddMulti 顺序错误：got %v", addedMulti)
		}
		if state := rc.luaState(); state != -1 {
			t.Fatalf("兜底路径不得改写记忆，got %d", state)
		}
	})
}

// TestBitmapMultiVsLoopConsistency 验证批量 Lua 脚本与逐条降级路径的语义等价：
// 同一批 items 分别经批量脚本（记忆 0/1 的正常路径）与逐条循环
// （记忆 -1 强制降级）写入两个独立 key，AddMulti/ExistsMulti 返回值序列
// （含顺序对应性）与最终位图必须完全一致——批量化的正确性回归。
func TestBitmapMultiVsLoopConsistency(t *testing.T) {
	rc, mr := newMiniRedisClient(t)
	ctx := context.Background()

	cfg := defaultBloomConfig()
	cfg.capacity = 10000
	items := []any{"x1", "x2", "x3", "x4", "x5", "x6", "x7"}

	// 批量脚本路径
	bBatch := newBitmapImpl(rc, "mv:batch", cfg)
	gotBatch, err := bBatch.AddMulti(ctx, items...)
	if err != nil {
		t.Fatalf("批量 AddMulti：%v", err)
	}
	if rc.luaState() != 1 {
		t.Fatalf("批量脚本成功应记忆支持 Lua，got %d", rc.luaState())
	}

	// 逐条降级路径（强制记忆 -1，入口跳过 EVAL）
	rc.luaMarkUnsupported()
	bLoop := newBitmapImpl(rc, "mv:loop", cfg)
	gotLoop, err := bLoop.AddMulti(ctx, items...)
	if err != nil {
		t.Fatalf("降级 AddMulti：%v", err)
	}
	if rc.luaState() != -1 {
		t.Fatalf("降级路径不得改写记忆，got %d", rc.luaState())
	}

	if len(gotBatch) != len(items) || len(gotLoop) != len(items) {
		t.Fatalf("返回长度不等于入参长度：%d / %d / %d", len(gotBatch), len(gotLoop), len(items))
	}
	for i := range items {
		if !gotBatch[i] || !gotLoop[i] {
			t.Fatalf("全新 items 第 %d 项应全为新增：batch=%v loop=%v", i, gotBatch[i], gotLoop[i])
		}
	}

	// 重复批量：与入参顺序对应的"已存在"（验证批量语义而非全 false 假通过）
	again := []any{"x7", "nope", "x3"}
	ra, err := bBatch.AddMulti(ctx, again...)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := bLoop.AddMulti(ctx, again...)
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{false, true, false}
	if fmt.Sprint(ra) != fmt.Sprint(want) || fmt.Sprint(rb) != fmt.Sprint(want) {
		t.Fatalf("重复批量顺序语义错误：batch=%v loop=%v want %v", ra, rb, want)
	}

	// ExistsMulti 两路径一致 + 顺序对应
	query := []any{"nope", "x1", "zzz", "x7"}
	ea, err := bBatch.ExistsMulti(ctx, query...)
	if err != nil {
		t.Fatal(err)
	}
	eb, err := bLoop.ExistsMulti(ctx, query...)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ea) != fmt.Sprint(eb) {
		t.Fatalf("ExistsMulti 两路径不一致：batch=%v loop=%v", ea, eb)
	}
	// 已灌入项必为 true（布隆无假阴性，且顺序对应：x1=idx1、x7=idx3）；
	// 未灌入项（nope/zzz）允许假阳性，不断言具体值。
	if ea[1] != true || ea[3] != true {
		t.Fatalf("ExistsMulti 已灌入项必为 true：got %v（query %v）", ea, query)
	}

	// 位图最终内容一致
	bm1, err1 := mr.Get("mv:batch")
	bm2, err2 := mr.Get("mv:loop")
	if err1 != nil || err2 != nil {
		t.Fatalf("读取位图失败：%v / %v", err1, err2)
	}
	if bm1 != bm2 {
		t.Fatalf("批量与逐条位图最终状态不一致（len %d vs %d）", len(bm1), len(bm2))
	}
}

// --- Redis Cluster 分片适配（bloom_shard.go） ---

// TestShardIndexRouting 验证路由纯函数（规格 10b）：确定性（同 item 多次
// 调用同结果）、模映射（idx == xxh3.Hash128(item).Hi % n）、值域
// [0, n)、n<=1 恒 0，以及大样本下的均匀散布（每桶 600~1400 / 期望 1000，
// 容差 ±40%，xxh3 实测远优于此）。
func TestShardIndexRouting(t *testing.T) {
	items := make([]string, 1000)
	for i := range items {
		items[i] = fmt.Sprintf("route-item-%d", i)
	}

	// 确定性 + 模映射 + 值域
	for n := 1; n <= 8; n++ {
		for _, item := range items[:200] {
			first := shardIndex(routeBytes(item), n)
			if first < 0 || first >= n {
				t.Fatalf("n=%d 值域越界：item=%s idx=%d", n, item, first)
			}
			for range 2 {
				if got := shardIndex(routeBytes(item), n); got != first {
					t.Fatalf("路由不确定：n=%d item=%s 两次结果 %d != %d", n, item, got, first)
				}
			}
			var want int
			if n > 1 {
				want = int(xxh3.Hash128([]byte(item)).Hi % uint64(n))
			}
			if first != want {
				t.Fatalf("模映射不符：n=%d item=%s got %d want %d", n, item, first, want)
			}
		}
	}

	// n<=1 恒 0（退化路径）
	for _, item := range items {
		if got := shardIndex(routeBytes(item), 1); got != 0 {
			t.Fatalf("n=1 应恒 0，got %d", got)
		}
		if got := shardIndex(routeBytes(item), 0); got != 0 {
			t.Fatalf("n=0 防御性应恒 0，got %d", got)
		}
	}

	// 均匀散布：8000 items × 8 桶
	const n, total = 8, 8000
	buckets := make([]int, n)
	for i := range total {
		buckets[shardIndex(routeBytes(fmt.Sprintf("uniform-%d", i)), n)]++
	}
	exp := total / n
	for i, c := range buckets {
		if c < exp*60/100 || c > exp*140/100 {
			t.Fatalf("均匀性异常：n=%d 桶 %d 计数 %d（期望 %d ±40%%）", n, i, c, exp)
		}
	}
}

// TestBloomShardCapacitySplit 验证容量分摊与收缩硬条件（规格 3）：
// effectiveN = min(requested, total/1000)，每分片容量 ceil(total/n) ≥ 1000
// （或 n==1 退化）；capacity<N、<2000、整除/进位边界全覆盖。并断言
// perShard 驱动的 m/k 不坍缩——堵死"整除为 0 → m=0→1、k=30、30 位全
// 挤同一位"的静默失真路径。
func TestBloomShardCapacitySplit(t *testing.T) {
	cases := []struct {
		name    string
		req     int
		total   int64
		wantN   int
		wantPer int64
	}{
		{"请求 8 片整容量", 8, 1_000_000, 8, 125_000},
		{"容量只够 5 片（capacity<N 收缩）", 8, 5_000, 5, 1_000},
		{"容量只够 4 片（进位）", 8, 4_500, 4, 1_125},
		{"恰好 2 片下限", 8, 2_000, 2, 1_000},
		{"总容量 <2000 退化 1 片", 8, 1_999, 1, 1_999},
		{"单片容量恰好 1000", 8, 1_000, 1, 1_000},
		{"总容量 < 下限仍可用（n=1）", 8, 999, 1, 999},
		{"请求 1 片（合法退化）", 1, 1_000_000, 1, 1_000_000},
		{"请求数超容量商", 1000, 10_000, 10, 1_000},
		{"进位收缩：5001/1000=5 → ceil(5001/5)", 8, 5_001, 5, 1_001}, // 5001/1000=5 → n=5 → ceil(5001/5)=1001
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := bloomEffectiveShardCount(c.req, c.total)
			if n != c.wantN {
				t.Fatalf("effectiveN got %d want %d（total=%d req=%d）", n, c.wantN, c.total, c.req)
			}
			per := bloomPerShardCapacity(c.total, n)
			if per != c.wantPer {
				t.Fatalf("perShard got %d want %d", per, c.wantPer)
			}
			// 硬条件：多分片时每分片容量 ≥ 1000
			if n > 1 && per < minShardCapacity {
				t.Fatalf("每分片容量 %d 低于下限 %d（n=%d total=%d）", per, minShardCapacity, n, c.total)
			}
			// m/k 不坍缩：位图至少覆盖每分片容量量级、k 在 [1,30]
			m := bloomBitCount(per, 0.01) | 1
			if m < 2 {
				t.Fatalf("m 坍缩为 %d（per=%d），失真路径未被堵死", m, per)
			}
			if k := bloomHashCount(per, m); k < 1 || k > 30 {
				t.Fatalf("k=%d 越出 [1,30]（per=%d m=%d）", k, per, m)
			}
		})
	}

	// MaxInt64 溢出回归（评审整改③）：ceil 必须用商余式——
	// (total+n-1)/n 在 total 逼近 MaxInt64 时加法回绕，产生错误极小值。
	t.Run("MaxInt64 溢出回归", func(t *testing.T) {
		// MaxInt64 = 2^63-1 = 8×(2^60-1)+7 → ceil(/8) = 2^60
		if got := bloomPerShardCapacity(math.MaxInt64, 8); got != 1<<60 {
			t.Fatalf("ceil(MaxInt64/8) got %d want %d（加法回绕溢出回归）", got, int64(1)<<60)
		}
		// MaxInt64 = 3×(2^63/3 取整)+2 → ceil(/3) = MaxInt64/3 + 1
		if got, want := bloomPerShardCapacity(math.MaxInt64, 3), int64(math.MaxInt64)/3+1; got != want {
			t.Fatalf("ceil(MaxInt64/3) got %d want %d", got, want)
		}
		// 收缩端同样不 panic/不溢出：MaxInt64 总容量 → effectiveN=requested
		if n := bloomEffectiveShardCount(8, math.MaxInt64); n != 8 {
			t.Fatalf("MaxInt64 总容量应收缩为请求值 8，got %d", n)
		}
	})
}

// TestResolveBloomShardingModes 表驱动验证分片触发边界（v0.5.0 起 opt-in
// 语义）：非集群、或集群但未显式请求（req<=1）关闭且键名无后缀（零回归）；
// 集群 req>1 才开启——含容量收缩退化态（effectiveN 缩到 1 时 enabled 仍
// 为 true、键名带 #0，保持集群开启态命名连续）。
func TestResolveBloomShardingModes(t *testing.T) {
	cases := []struct {
		name     string
		mode     Mode
		req      int
		total    int64
		wantEn   bool
		wantN    int
		wantPer  int64
		wantKey0 string // 首分片物理键（关闭态为无后缀 base）
	}{
		// 非集群：即使显式 WithShardCount(8) 也关闭，键名与 standalone 一致
		{"standalone+8 关闭", ModeStandalone, 8, 100_000, false, 1, 100_000, "base"},
		{"sentinel+8 关闭", ModeSentinel, 8, 100_000, false, 1, 100_000, "base"},
		{"ring+8 关闭", ModeRing, 8, 100_000, false, 1, 100_000, "base"},
		// 集群但未显式请求（默认值 1）：关闭，键名无后缀
		{"cluster+默认(1) 关闭", ModeCluster, defaultBloomShardCount, 100_000, false, 1, 100_000, "base"},
		// 集群显式请求 >1：开启、按容量分摊
		{"cluster+8 开启", ModeCluster, 8, 100_000, true, 8, 12_500, "base#0"},
		// 集群请求 2、容量收缩到 effectiveN=1：仍开启、键带 #0（退化态连续性）
		{"cluster+2 容量1500 退化仍开启", ModeCluster, 2, 1_500, true, 1, 1_500, "base#0"},
		// 集群请求 8、容量收缩到 effectiveN=1：仍开启、键带 #0
		{"cluster+8 容量1500 退化仍开启", ModeCluster, 8, 1_500, true, 1, 1_500, "base#0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enabled, n, per := resolveBloomSharding(c.mode, c.req, c.total)
			if enabled != c.wantEn || n != c.wantN || per != c.wantPer {
				t.Fatalf("resolveBloomSharding(%v,%d,%d) got (en=%v n=%d per=%d)，want (en=%v n=%d per=%d)",
					c.mode, c.req, c.total, enabled, n, per, c.wantEn, c.wantN, c.wantPer)
			}
			// 键名口径：关闭态恒无后缀 base；开启态（含退化 n=1）首片带 #0
			s := newBloomSharder("base", enabled, n)
			if got := s.shardKey(0); got != c.wantKey0 {
				t.Fatalf("首分片键名 got %q want %q（enabled=%v n=%d）", got, c.wantKey0, enabled, n)
			}
		})
	}
}

// TestBloomSharderKeysAndGroup 验证键名与分组（规格 1/2/5）：分片态键名
// <base>#<idx>（含退化态恒带 #0）、关闭态恒 base 零回归；group 保留原始
// 下标、按 idx 升序、每项落对分组。
func TestBloomSharderKeysAndGroup(t *testing.T) {
	t.Run("关闭态零回归", func(t *testing.T) {
		s := newBloomSharder("plain", false, 8)
		if got := s.shardKey(3); got != "plain" {
			t.Fatalf("关闭态 shardKey 应恒为 base，got %q", got)
		}
		if got := s.keyFor(routeBytes("anything")); got != "plain" {
			t.Fatalf("关闭态 keyFor 应为 base，got %q", got)
		}
		if keys := s.allKeys(); len(keys) != 1 || keys[0] != "plain" {
			t.Fatalf("关闭态 allKeys got %v", keys)
		}
		groups, err := s.group([]any{"a", "b", "c"})
		if err != nil {
			t.Fatalf("关闭态 group：%v", err)
		}
		if len(groups) != 1 || groups[0].key != "plain" {
			t.Fatalf("关闭态 group 应单组 base，got %+v", groups)
		}
		for i, idx := range groups[0].srcIdx {
			if idx != i {
				t.Fatalf("单组 srcIdx 应为恒等序列，got %v", groups[0].srcIdx)
			}
		}
	})

	t.Run("退化态带 #0 后缀", func(t *testing.T) {
		s := newBloomSharder("base", true, 1)
		if got := s.shardKey(0); got != "base#0" {
			t.Fatalf("集群退化态键名应为 base#0，got %q", got)
		}
		groups, err := s.group([]any{"x", "y"})
		if err != nil {
			t.Fatalf("退化态 group：%v", err)
		}
		if len(groups) != 1 || groups[0].key != "base#0" {
			t.Fatalf("退化态 group 键名应为 base#0，got %+v", groups)
		}
	})

	t.Run("多分片键名与散布", func(t *testing.T) {
		s := newBloomSharder("base", true, 4)
		want := []string{"base#0", "base#1", "base#2", "base#3"}
		keys := s.allKeys()
		if fmt.Sprint(keys) != fmt.Sprint(want) {
			t.Fatalf("allKeys got %v want %v", keys, want)
		}
		seen := map[string]bool{}
		for i := range 500 {
			item := fmt.Sprintf("sk-%d", i)
			k := s.keyFor(routeBytes(item))
			seen[k] = true
			if want := "base#" + fmt.Sprint(shardIndex(routeBytes(item), 4)); k != want {
				t.Fatalf("keyFor(%s) got %s want %s", item, k, want)
			}
		}
		if len(seen) != 4 {
			t.Fatalf("500 个样本应散布到 4 个分片，got %d", len(seen))
		}
	})

	t.Run("分组保留原始下标", func(t *testing.T) {
		s := newBloomSharder("g", true, 6)
		items := make([]string, 300)
		for i := range items {
			items[i] = fmt.Sprintf("grp-%d", i)
		}
		groups, err := s.group(anyItems(items))
		if err != nil {
			t.Fatalf("group：%v", err)
		}
		if len(groups) == 0 || len(groups) > 6 {
			t.Fatalf("分组数异常：%d", len(groups))
		}
		prevIdx := -1
		covered := map[int]bool{}
		for _, g := range groups {
			if g.idx <= prevIdx {
				t.Fatalf("分组未按 idx 升序：%d 出现在 %d 之后", g.idx, prevIdx)
			}
			prevIdx = g.idx
			if g.key != "g#"+fmt.Sprint(g.idx) {
				t.Fatalf("分组键名错误：%q", g.key)
			}
			for j, item := range g.items {
				// group 组内保留原始 any（不改写为路由字节），断言可安全还原
				sItem, ok := item.(string)
				if !ok {
					t.Fatalf("分组内 item 应为原始 any，got %T", item)
				}
				if shardIndex(routeBytes(sItem), 6) != g.idx {
					t.Fatalf("item %s 落错分组（idx=%d）", sItem, g.idx)
				}
				orig := g.srcIdx[j]
				if items[orig] != sItem {
					t.Fatalf("srcIdx[%d]=%d 指回错误 item：%q != %q", j, orig, items[orig], sItem)
				}
				covered[orig] = true
			}
		}
		if len(covered) != len(items) {
			t.Fatalf("分组丢失条目：覆盖 %d，期望 %d", len(covered), len(items))
		}
	})
}

// TestWithShardCountOption 验证 WithShardCount 选项语义（规格 9）：
// 默认 1（即关闭分片）；n<=0 静默忽略保留默认；任意正整数合法（含 1）。
func TestWithShardCountOption(t *testing.T) {
	cfg := defaultBloomConfig()
	if cfg.shardCount != defaultBloomShardCount {
		t.Fatalf("默认分片数应为 %d，got %d", defaultBloomShardCount, cfg.shardCount)
	}
	WithShardCount(0)(&cfg)
	WithShardCount(-3)(&cfg)
	if cfg.shardCount != defaultBloomShardCount {
		t.Fatalf("非法值应静默忽略保留默认，got %d", cfg.shardCount)
	}
	WithShardCount(1)(&cfg)
	if cfg.shardCount != 1 {
		t.Fatalf("n=1 应合法采纳，got %d", cfg.shardCount)
	}
	WithShardCount(64)(&cfg)
	if cfg.shardCount != 64 {
		t.Fatalf("n=64 应采纳，got %d", cfg.shardCount)
	}
}

// newShardedBitmapForTest 在 miniredis（standalone）上构造**注入式分片**
// bitmapImpl，用于集群分片语义验证（规格 10a）：容量收缩与 m/k 分摊已由
// 纯函数测试覆盖，此处 cfg.capacity=1000 对应"每分片容量"，注入
// sharder{n:4} 后语义等价 cluster 下 total=4000 / effectiveN=4 的形态。
func newShardedBitmapForTest(t *testing.T, key string, policy FailPolicy) (*bitmapImpl, *redisClient, *miniredis.Miniredis) {
	t.Helper()
	rc, mr := newMiniRedisClient(t)
	cfg := defaultBloomConfig()
	cfg.capacity = 1000 // 即每分片容量（见函数注释）
	cfg.policy = policy
	b := newBitmapImpl(rc, key, cfg)
	b.sharder = newBloomSharder(key, true, 4)
	b.totalCapacity = cfg.capacity * 4 // 上报口径：全局总容量 4000
	return b, rc, mr
}

// TestBitmapShardedMiniredis 端到端验证 bitmap 分片路径（miniredis 注入
// shardN=4）：键名格式与散布、k 位落对分片、AddMulti/ExistsMulti 跨分片
// 结果顺序回填（与单条路径逐位一致）。
func TestBitmapShardedMiniredis(t *testing.T) {
	ctx := context.Background()
	const nShards = 4

	items := make([]any, 200)
	for i := range items {
		items[i] = fmt.Sprintf("sd-item-%d", i)
	}

	t.Run("键名格式与散布", func(t *testing.T) {
		b, _, mr := newShardedBitmapForTest(t, "shard:keys", FailOpen)
		res, err := b.AddMulti(ctx, items...)
		if err != nil {
			t.Fatalf("分片 AddMulti：%v", err)
		}
		for i, v := range res {
			if !v {
				t.Fatalf("全新 item %s 应判新增，got false", items[i])
			}
		}

		// 只允许出现 <base>#<idx> 形态的键（不得有裸 base 键），且
		// 200 个 item 足以覆盖全部 4 个分片
		allow := map[string]bool{}
		for i := range nShards {
			allow[fmt.Sprintf("shard:keys#%d", i)] = true
		}
		present := map[string]bool{}
		for _, k := range mr.Keys() {
			if !allow[k] {
				t.Fatalf("出现非法键名 %q（期望仅 %v 形态）", k, allow)
			}
			present[k] = true
		}
		for k := range allow {
			if !present[k] {
				t.Fatalf("分片键 %s 未被散布命中（路由不均或键名错误）", k)
			}
		}
	})

	t.Run("k 位落在正确分片", func(t *testing.T) {
		b, rc, _ := newShardedBitmapForTest(t, "shard:pos", FailOpen)
		if _, err := b.AddMulti(ctx, items...); err != nil {
			t.Fatal(err)
		}
		// 抽样：item 的 k 个位置在路由分片键上必须全部为 1
		for _, item := range items[:25] {
			data, err := marshalItem(item)
			if err != nil {
				t.Fatalf("marshalItem(%v)：%v", item, err)
			}
			key := b.sharder.keyFor(data)
			positions, err := b.hashs(item)
			if err != nil {
				t.Fatalf("hashs(%v)：%v", item, err)
			}
			for _, pos := range positions {
				v, err := rc.GetBit(ctx, key, int64(pos)).Result()
				if err != nil {
					t.Fatalf("GetBit(%s, %d)：%v", key, pos, err)
				}
				if v != 1 {
					t.Fatalf("item %s 的位 %d 未落在路由分片键 %s（跨片写失败或路由分叉）", item, pos, key)
				}
			}
		}
	})

	t.Run("批量与单条结果顺序一致", func(t *testing.T) {
		b, _, _ := newShardedBitmapForTest(t, "shard:order", FailOpen)
		if _, err := b.AddMulti(ctx, items...); err != nil {
			t.Fatal(err)
		}
		// 重复 AddMulti：已存在条目必须全 false（验证按原始下标回填而非
		// 分组内顺序直写）
		again, err := b.AddMulti(ctx, items...)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range again {
			if v {
				t.Fatalf("重复 AddMulti[%d]（%s）应 false，回填顺序错位", i, items[i])
			}
		}

		// 交错"已灌入/未灌入"的混合查询：ExistsMulti 第 i 位必须与单条
		// Exists（同一 sharder 路由）一致——跨分片分组回填的正确性回归
		mix := make([]any, 0, 60)
		for i := range 40 {
			mix = append(mix, items[i*3], fmt.Sprintf("sd-missing-%d", i))
		}
		multi, err := b.ExistsMulti(ctx, mix...)
		if err != nil {
			t.Fatalf("分片 ExistsMulti：%v", err)
		}
		for i, item := range mix {
			single, err := b.Exists(ctx, item)
			if err != nil {
				t.Fatalf("单条 Exists(%s)：%v", item, err)
			}
			if multi[i] != single {
				t.Fatalf("ExistsMulti[%d]（%s）与单条结果不一致：batch=%v single=%v（回填错位）", i, item, multi[i], single)
			}
			if i%2 == 0 && !multi[i] {
				t.Fatalf("已灌入 item %s 出现假阴性（布隆不允许）", item)
			}
		}
	})

	t.Run("Info 聚合含空分片", func(t *testing.T) {
		b, _, _ := newShardedBitmapForTest(t, "shard:info", FailOpen)
		sub := items[:30]
		if _, err := b.AddMulti(ctx, sub...); err != nil {
			t.Fatal(err)
		}
		info, err := b.Info(ctx)
		if err != nil {
			t.Fatalf("分片 Info：%v", err)
		}
		if info.Capacity != 4000 {
			t.Fatalf("Info.Capacity 应上报配置总容量 4000，got %d", info.Capacity)
		}
		// 30 个全新 item：逐分片独立估计求和应接近 30（宽松区间防估计噪声）
		if info.NumItems < 20 || info.NumItems > 45 {
			t.Fatalf("NumItems 聚合估计异常：got %d（灌入 30）", info.NumItems)
		}
		if info.Size <= 0 {
			t.Fatalf("Size 应为各分片 StrLen 之和（>0），got %d", info.Size)
		}

		// 空分片：仅灌 1 个 item，其余分片 BITCOUNT 天然零值，聚合不报错
		b1, _, _ := newShardedBitmapForTest(t, "shard:info1", FailOpen)
		if _, err := b1.Add(ctx, "only-one"); err != nil {
			t.Fatal(err)
		}
		info1, err := b1.Info(ctx)
		if err != nil {
			t.Fatalf("含空分片的 Info 不应报错：%v", err)
		}
		if info1.NumItems < 1 || info1.NumItems > 2 {
			t.Fatalf("单 item 聚合估计应 ≈1，got %d", info1.NumItems)
		}
	})
}

// TestBitmapShardedLoopVsBatchConsistency 验证分片降级路径（Lua 禁用后
// 逐条串行，含 pipeline 位操作兜底）与分片批量路径（Pipeline 内多分片
// EVAL）的结果序列与每分片位图内容完全一致（规格 10a）。两侧用独立
// miniredis 实例（luaSupport 为 client 级记忆，共享会互相污染）。
func TestBitmapShardedLoopVsBatchConsistency(t *testing.T) {
	ctx := context.Background()
	items := make([]any, 120)
	for i := range items {
		items[i] = fmt.Sprintf("lbc-%d", i)
	}

	bBatch, _, mrA := newShardedBitmapForTest(t, "shard:cons", FailOpen)
	bLoop, rcB, mrB := newShardedBitmapForTest(t, "shard:cons", FailOpen)
	rcB.luaMarkUnsupported() // 入口跳过 EVAL → 逐条降级（不可再套外层 pipeline）

	ra, err := bBatch.AddMulti(ctx, items...)
	if err != nil {
		t.Fatalf("批量分片 AddMulti：%v", err)
	}
	rb, err := bLoop.AddMulti(ctx, items...)
	if err != nil {
		t.Fatalf("降级分片 AddMulti：%v", err)
	}
	if fmt.Sprint(ra) != fmt.Sprint(rb) {
		t.Fatalf("AddMulti 结果序列不一致：batch=%v loop=%v", ra, rb)
	}
	for _, v := range ra {
		if !v {
			t.Fatal("全新 items 批量路径出现 false（语义异常）")
		}
	}

	// 重复批量（已存在条目应全 false 且两侧一致）
	qa := []any{items[5], "lbc-nope-1", items[60]}
	aa, err := bBatch.AddMulti(ctx, qa...)
	if err != nil {
		t.Fatal(err)
	}
	ab, err := bLoop.AddMulti(ctx, qa...)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(aa) != fmt.Sprint(ab) {
		t.Fatalf("重复批量结果不一致：batch=%v loop=%v", aa, ab)
	}

	// ExistsMulti 两路径一致
	qu := []any{"lbc-nope-1", items[0], items[7], "lbc-nope-2"}
	ea, err := bBatch.ExistsMulti(ctx, qu...)
	if err != nil {
		t.Fatal(err)
	}
	eb, err := bLoop.ExistsMulti(ctx, qu...)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ea) != fmt.Sprint(eb) {
		t.Fatalf("ExistsMulti 两路径不一致：batch=%v loop=%v", ea, eb)
	}
	if ea[1] != true || ea[2] != true {
		t.Fatalf("已灌入项 ExistsMulti 必须为 true（假阴性）：%v", ea)
	}

	// 键集合一致 + 每个分片位图逐字节一致（两侧路由同码，键名必相同）
	ka := append([]string(nil), mrA.Keys()...)
	kb := append([]string(nil), mrB.Keys()...)
	if fmt.Sprint(ka) != fmt.Sprint(kb) {
		t.Fatalf("两路径分片键集合不一致：batch=%v loop=%v", ka, kb)
	}
	if len(ka) == 0 {
		t.Fatal("未写入任何分片键")
	}
	for _, k := range ka {
		sa, err := mrA.Get(k)
		if err != nil {
			t.Fatal(err)
		}
		sb, err := mrB.Get(k)
		if err != nil {
			t.Fatal(err)
		}
		if sa != sb {
			t.Fatalf("分片键 %s 位图内容不一致（len %d vs %d）", k, len(sa), len(sb))
		}
	}
}

// TestBloomShardedUnavailableWholeFallback 验证分片下 FailOpen/FailClosed
// 的"整体失败"定义（规格 8）：任一分片组服务不可用 → 全部 items 按
// policy 兜底 + ErrRedisUnavailable 哨兵错误，禁止部分真实部分兜底。
func TestBloomShardedUnavailableWholeFallback(t *testing.T) {
	ctx := context.Background()
	items := make([]any, 50)
	for i := range items {
		items[i] = fmt.Sprintf("uf-%d", i)
	}

	check := func(policy FailPolicy) {
		t.Helper()
		b, _, mr := newShardedBitmapForTest(t, "shard:uf", policy)
		// 先灌一半建立"真实结果"，再宕掉服务器：残余批量必须整体兜底，
		// 不得出现"已存在分片返回真实值、失联分片返回兜底值"的混合
		half := items[:25]
		if _, err := b.AddMulti(ctx, half...); err != nil {
			t.Fatal(err)
		}
		mr.Close()

		want := policy == FailOpen // FailOpen → 全 true；FailClosed → 全 false
		got, err := b.AddMulti(ctx, items...)
		if !errors.Is(err, ErrRedisUnavailable) {
			t.Fatalf("policy=%v 分片不可用应返回哨兵错误，got %v", policy, err)
		}
		if len(got) != len(items) {
			t.Fatalf("兜底切片长度 got %d want %d", len(got), len(items))
		}
		for i, v := range got {
			if v != want {
				t.Fatalf("policy=%v 第 %d 项兜底值 got %v want %v（疑似混合结果）", policy, i, v, want)
			}
		}
		got2, err := b.ExistsMulti(ctx, items...)
		if !errors.Is(err, ErrRedisUnavailable) || len(got2) != len(items) {
			t.Fatalf("policy=%v ExistsMulti 整体兜底失败：len=%d err=%v", policy, len(got2), err)
		}
		for i, v := range got2 {
			if v != want {
				t.Fatalf("policy=%v ExistsMulti[%d] got %v want %v", policy, i, v, want)
			}
		}
	}

	check(FailOpen)
	check(FailClosed)
}

// TestBloomShardRoutingSharedByBothImpls 验证 BF/bitmap 两 impl 的分片
// 共享层行为一致（规格 6）：同一 sharder 参数下 keyFor/shardKey/group
// 逐条目、逐分组完全相同——路由/分组逻辑只有一份实现。
func TestBloomShardRoutingSharedByBothImpls(t *testing.T) {
	s := newBloomSharder("shared", true, 6)
	bf := &bfCmdImpl{sharder: s}
	bm := &bitmapImpl{sharder: s}

	items := make([]string, 300)
	for i := range items {
		items[i] = fmt.Sprintf("shared-%d", i)
	}
	for _, item := range items {
		// 与生产路径同口径：路由入参是 marshalItem 的规范字节
		data, err := marshalItem(item)
		if err != nil {
			t.Fatalf("marshalItem(%s)：%v", item, err)
		}
		if bf.sharder.keyFor(data) != bm.sharder.keyFor(data) {
			t.Fatalf("两路径 keyFor(%s) 分叉：%q vs %q", item,
				bf.sharder.keyFor(data), bm.sharder.keyFor(data))
		}
		if bf.sharder.indexOf(data) != bm.sharder.indexOf(data) {
			t.Fatalf("两路径 indexOf(%s) 分叉", item)
		}
	}
	ga, err := bf.sharder.group(anyItems(items))
	if err != nil {
		t.Fatalf("bf group：%v", err)
	}
	gb, err := bm.sharder.group(anyItems(items))
	if err != nil {
		t.Fatalf("bm group：%v", err)
	}
	if len(ga) != len(gb) {
		t.Fatalf("分组数不一致：%d vs %d", len(ga), len(gb))
	}
	for i := range ga {
		if ga[i].key != gb[i].key || ga[i].idx != gb[i].idx ||
			fmt.Sprint(ga[i].items) != fmt.Sprint(gb[i].items) ||
			fmt.Sprint(ga[i].srcIdx) != fmt.Sprint(gb[i].srcIdx) {
			t.Fatalf("第 %d 组分组结果不一致", i)
		}
	}
}

// --- 双实现路径 A/B 对照（白盒直构） ---

// TestBloomABRealRedis 在真实 Redis 上白盒直构 bfCmdImpl 与 bitmapImpl
// 两条路径做 A/B 对照（规格 5c）：两条独立 key、同容量同参数，
// 各跑 Add/AddMulti/Exists/ExistsMulti——每路径 1000 元素自洽（布隆无
// 假阴性，已灌入项必须全 true）+ Info 合理性断言。**不做跨路径的严格
// 对比断言**（BF 自动扩容 vs bitmap 固定布局、ItemsInserted 精确值 vs
// 估计值，逐项相等必 flaky）。
// 守卫：REDIS_URL 未设置或服务器无 bf 模块（RedisBloom 部署 / Redis 8.x
// community 内置）时跳过——无模块环境无法构造 BF 路径对照。
// 共享实例纪律：key 带 bloomtest:<随机> 前缀，收尾 Del，严禁 FLUSHDB。
func TestBloomABRealRedis(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skip real-Redis A/B test")
	}
	rdb, err := NewWithUrl(url)
	if err != nil {
		t.Fatalf("NewWithUrl: %v", err)
	}
	defer func() { _ = rdb.GracefulClose(context.Background()) }()

	if !rdb.Capability().HasModule("bf") {
		t.Skip("服务器未加载 bf 模块（RedisBloom / Redis 8.x community），跳过 A/B 对照")
	}

	ctx := context.Background()
	keyBF := bloomTestKey("ab-bf")
	keyBMP := bloomTestKey("ab-bmp")
	defer func() {
		_ = rdb.Del(ctx, keyBF, keyBMP).Err()
	}()

	const n = 1000
	items := make([]any, n)
	for i := range items {
		items[i] = fmt.Sprintf("ab-%s-%d", keyBF, i)
	}

	runPath := func(name string, f BloomFilter) {
		t.Helper()
		// "自洽"判据 = 写入必可读（布隆无假阴性是硬保证）。
		// **不断言全新元素 Add/AddMulti 必返回 true**：返回 false 只表示
		// 服务端判"可能已存在"（正常假阳性事件，位仍已写入）。BF
		// standalone 路径不预分配（见 NewBloomFilterWithEstimate 注释），
		// 1000 元素灌进 RedisBloom 默认 capacity=100 的子过滤器扩容链时，
		// 新增判定的假阳性率达百分位——属两条路径既有容量语义差异，非缺陷。
		for i := range n / 2 {
			if _, err := f.Add(ctx, items[i]); err != nil {
				t.Fatalf("%s Add(%s)：%v", name, items[i], err)
			}
		}
		res, err := f.AddMulti(ctx, items[n/2:]...)
		if err != nil {
			t.Fatalf("%s AddMulti：%v", name, err)
		}
		if len(res) != n/2 {
			t.Fatalf("%s AddMulti 返回长度 got %d want %d（顺序对应性破坏）", name, len(res), n/2)
		}
		// 单条 Exists 抽查（两端 + 中段）
		for _, idx := range []int{0, n/2 - 1, n / 2, n - 1} {
			ok, err := f.Exists(ctx, items[idx])
			if err != nil || !ok {
				t.Fatalf("%s Exists(%s)：ok=%v err=%v（假阴性不允许）", name, items[idx], ok, err)
			}
		}
		// ExistsMulti 全量自查：无假阴性 + 顺序对应
		all, err := f.ExistsMulti(ctx, items...)
		if err != nil {
			t.Fatalf("%s ExistsMulti：%v", name, err)
		}
		if len(all) != n {
			t.Fatalf("%s ExistsMulti 返回长度 got %d want %d", name, len(all), n)
		}
		for i, v := range all {
			if !v {
				t.Fatalf("%s ExistsMulti[%d]（%s）假阴性", name, i, items[i])
			}
		}
	}

	// 白盒直构两条实现路径（不经工厂分派，与服务器是否加载 bf 模块无关
	// ——对照语义由构造保证，不再依赖已删除的强制路径 Option）
	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}
	cfg := defaultBloomConfig()
	WithCapacity(10_000)(&cfg)
	WithFalsePositive(0.01)(&cfg)
	cfg.policy = FailOpen // 工厂 NewBloomFilter 的原默认赋值，直构需显式补
	bf := rc.newBFImpl(keyBF, cfg)
	bmp := newBitmapImpl(rc, keyBMP, cfg)

	runPath("BF.*", bf)
	runPath("bitmap", bmp)

	// Info 合理性（不做跨路径严格对比）：NumItems 在 1000 ±10% 内、
	// Size/Capacity 为正。BF 路径 ItemsInserted 为精确计数，bitmap 路径
	// 是置位数估计——两者都应在宽松区间内；BF standalone 无预分配
	// （见 NewBloomFilterWithEstimate 注释），Capacity 为扩容后的子过滤器
	// 容量和，仅断言 >0。
	checkInfo := func(name string, f BloomFilter) {
		t.Helper()
		info, err := f.Info(ctx)
		if err != nil {
			t.Fatalf("%s Info：%v", name, err)
		}
		if info.NumItems < n*90/100 || info.NumItems > n*110/100 {
			t.Fatalf("%s Info.NumItems got %d，超出 1000±10%%（灌入 %d）", name, info.NumItems, n)
		}
		if info.Capacity <= 0 || info.Size <= 0 {
			t.Fatalf("%s Info 异常：%+v", name, info)
		}
		t.Logf("%s Info：%+v", name, info)
	}
	checkInfo("BF.*", bf)
	checkInfo("bitmap", bmp)
}

// --- Reset（清空物理键 + 惰性 RESERVE 闸门复位） ---

// bfGatePtrs 抓取 bfCmdImpl 各分片闸门当前指向的 *sync.Once，用于断言
// Reset 前后指针换代（白盒验证闸门复位）。
func bfGatePtrs(bf *bfCmdImpl) []*sync.Once {
	ptrs := make([]*sync.Once, len(bf.reserves))
	for i := range bf.reserves {
		ptrs[i] = bf.reserves[i].Load()
	}
	return ptrs
}

// newShardedBFOnMini 在 miniredis 上手工构造分片形态（enabled，n=4）的
// bfCmdImpl——本库包测试环境无 RedisBloom 模块（miniredis 不支持 BF.*），
// 也不具备真实集群；本构造专供白盒验证 Reset 中**不依赖 BF.* 的部分**：
// DEL 清空、atomic.Pointer 闸门的换代与重新武装（reserveShard 会真实
// 发出 BF.RESERVE 并收到 unknown command 错误——错误形态恰是可观测信号）。
// 端到端的 RESERVE 参数正确性（Capacity==perShard）见
// TestBloomResetBFCapacityRealRedisBloom（需真实环境，守卫跳过）。
func newShardedBFOnMini(t *testing.T, key string, policy FailPolicy) (*bfCmdImpl, *redisClient, *miniredis.Miniredis) {
	t.Helper()
	rc, mr := newMiniRedisClient(t)
	cfg := defaultBloomConfig()
	cfg.policy = policy
	bf := &bfCmdImpl{
		client:   rc,
		cfg:      cfg,
		policy:   policy,
		sharder:  newBloomSharder(key, true, 4),
		perShard: 1000,
	}
	bf.reserves = make([]atomic.Pointer[sync.Once], 4)
	for i := range bf.reserves {
		bf.reserves[i].Store(new(sync.Once))
	}
	return bf, rc, mr
}

// TestBloomResetBitmapStandalone 验证 bitmap 单键（未分片）形态 Reset：
// Add 若干 → Reset → 同 item Exists==false（假阴性不允许：清空后必须
// 全 false）→ 重写判新增；m/k 布局（实例常量）不受 Reset 影响；
// Info 对已删键返回零值（StrLen/BITCOUNT 天然 0，非 not-found 错误——
// 既有行为不变）。
func TestBloomResetBitmapStandalone(t *testing.T) {
	ctx := context.Background()
	rc, mr := newMiniRedisClient(t)
	cfg := defaultBloomConfig()
	cfg.capacity = 10000
	cfg.falsePositive = 0.01
	cfg.policy = FailOpen
	b := newBitmapImpl(rc, "rst:solo", cfg)
	if b.sharder.enabled {
		t.Fatalf("miniredis standalone 不应启用分片")
	}

	items := []any{"a", "b", "c"}
	res, err := b.AddMulti(ctx, items...)
	if err != nil {
		t.Fatalf("AddMulti：%v", err)
	}
	for i, v := range res {
		if !v {
			t.Fatalf("全新 item %v 应判新增", items[i])
		}
	}
	if !mr.Exists("rst:solo") {
		t.Fatal("Add 后物理键应存在")
	}
	infoBefore, err := b.Info(ctx)
	if err != nil || infoBefore.NumItems < 1 {
		t.Fatalf("Reset 前 Info 应 >0：got %+v err=%v", infoBefore, err)
	}

	if err := b.Reset(ctx); err != nil {
		t.Fatalf("单键 Reset：%v", err)
	}
	if mr.Exists("rst:solo") {
		t.Fatal("Reset 后物理键仍存在（DEL 未生效）")
	}
	// Info 对不存在键零值不报错（standalone 既有 not-found 行为不变）——
	// 必须在重写之前断言，否则新置位会污染计数。
	info, err := b.Info(ctx)
	if err != nil {
		t.Fatalf("standalone 空过滤器 Info 不应报错：%v", err)
	}
	if info.NumItems != 0 || info.Size != 0 {
		t.Fatalf("Reset 后 Info 计数应归零：got %+v", info)
	}
	for _, item := range items {
		ok, err := b.Exists(ctx, item)
		if err != nil {
			t.Fatalf("Reset 后 Exists(%v)：%v", item, err)
		}
		if ok {
			t.Fatalf("Reset 后 Exists(%v) 必须为 false（清空语义破坏）", item)
		}
	}
	// 重写判新增（键已被删除，等价全新过滤器）
	added, err := b.Add(ctx, "a")
	if err != nil || !added {
		t.Fatalf("Reset 后 Add(%v) 应判新增：added=%v err=%v", "a", added, err)
	}
	// m/k 布局是实例构造期常量，Reset 不得触碰
	if b.m != bloomBitCount(10000, 0.01)|1 || b.k != bloomHashCount(10000, b.m) {
		t.Fatalf("Reset 改变 m/k 布局：m=%d k=%d", b.m, b.k)
	}
}

// TestBloomResetBitmapSharded 验证 bitmap 分片形态 Reset：多分片写入 →
// Reset → 全部分片键不存在 → 重写路由正确（同 item 同分片、键名格式
// 不变）；分片态 Info 归零（空分片键 StrLen/BITCOUNT 天然 0，聚合不
// 报错）。
func TestBloomResetBitmapSharded(t *testing.T) {
	ctx := context.Background()
	b, _, mr := newShardedBitmapForTest(t, "rst:shard", FailOpen)

	items := make([]any, 200)
	for i := range items {
		items[i] = fmt.Sprintf("rst-item-%d", i)
	}
	if _, err := b.AddMulti(ctx, items...); err != nil {
		t.Fatal(err)
	}
	info, err := b.Info(ctx)
	if err != nil || info.NumItems <= 0 {
		t.Fatalf("Reset 前 Info 应 >0：got %+v err=%v", info, err)
	}

	if err := b.Reset(ctx); err != nil {
		t.Fatalf("分片 Reset：%v", err)
	}
	if ks := mr.Keys(); len(ks) != 0 {
		t.Fatalf("Reset 后仍残留键 %v（分片未清干净）", ks)
	}
	for _, item := range items[:50] {
		ok, err := b.Exists(ctx, item)
		if err != nil || ok {
			t.Fatalf("Reset 后 Exists(%v)：ok=%v err=%v", item, ok, err)
		}
	}
	// 重写路由正确：与 Reset 前同 sharder 口径
	for i := range 20 {
		_, err := b.Add(ctx, items[i])
		if err != nil {
			t.Fatal(err)
		}
		data, err := marshalItem(items[i])
		if err != nil {
			t.Fatal(err)
		}
		key := b.sharder.keyFor(data)
		if !mr.Exists(key) {
			t.Fatalf("重写未落路由分片键 %s（路由漂移）", key)
		}
	}
	// Info 归零（键全部删除后重新灌入少量前先单独验证一次全清口径）
	if err := b.Reset(ctx); err != nil {
		t.Fatalf("二次 Reset：%v", err)
	}
	info, err = b.Info(ctx)
	if err != nil {
		t.Fatalf("分片清空后 Info 不应报错：%v", err)
	}
	if info.NumItems != 0 {
		t.Fatalf("分片 Reset 后 NumItems 应为 0，got %d", info.NumItems)
	}
}

// TestBloomResetBFGateRearm 用 miniredis 白盒验证 bfCmdImpl.Reset 的
// 客户端逻辑：DEL 清空（单键与分片 pipeline 两形态）、闸门 Once 换代、
// 重新武装后 reserveShard 再次实际执行（BF.RESERVE 重发以 unknown
// command 错误为可观测信号——once 未换代则 Do 不再执行 f、错误消失）。
// BF.* 端到端参数正确性另见真环境守卫用例。
func TestBloomResetBFGateRearm(t *testing.T) {
	ctx := context.Background()

	t.Run("非分片闸门为 nil 且 Reset 仅删键", func(t *testing.T) {
		rc, mr := newMiniRedisClient(t)
		cfg := defaultBloomConfig()
		cfg.policy = FailOpen
		bf := rc.newBFImpl("rst:bfsolo", cfg) // miniredis standalone → enabled=false
		if bf.sharder.enabled || len(bf.reserves) != 0 {
			t.Fatal("standalone 不应启用分片/分配闸门")
		}
		if err := rc.Set(ctx, "rst:bfsolo", "x", 0).Err(); err != nil {
			t.Fatal(err)
		}
		if err := bf.Reset(ctx); err != nil {
			t.Fatalf("单键 Reset：%v", err)
		}
		if mr.Exists("rst:bfsolo") {
			t.Fatal("单键 Reset 后键残留")
		}
		// 闸门为 nil slice：复位循环空转，reserveShard 短路返回 nil
		if err := bf.reserveShard(ctx, 0); err != nil {
			t.Fatalf("非分片 reserveShard 应短路 nil，got %v", err)
		}
	})

	t.Run("分片 DEL 清空与闸门换代重新武装", func(t *testing.T) {
		bf, rc, mr := newShardedBFOnMini(t, "rst:bfshard", FailOpen)
		keys := bf.sharder.allKeys()
		if fmt.Sprint(keys) != "[rst:bfshard#0 rst:bfshard#1 rst:bfshard#2 rst:bfshard#3]" {
			t.Fatalf("分片键名口径异常：%v", keys)
		}
		for _, k := range keys {
			if err := rc.Set(ctx, k, "x", 0).Err(); err != nil {
				t.Fatal(err)
			}
		}

		// 先把闸门"烧掉"：miniredis 无 BF 模块，reserveShard 真实发出
		// BF.RESERVE 收到 unknown command（数据类错误、不命中 already
		// exists 吞错分支），once 燃尽且错误原样返回。
		for i := range keys {
			err := bf.reserveShard(ctx, i)
			if err == nil {
				t.Fatalf("无 bf 模块环境 reserveShard(%d) 应报 unknown command", i)
			}
			if errors.Is(err, ErrRedisUnavailable) {
				t.Fatalf("unknown command 是数据类错误，不得走兜底哨兵：%v", err)
			}
		}
		before := bfGatePtrs(bf)

		if err := bf.Reset(ctx); err != nil {
			t.Fatalf("分片 Reset：%v", err)
		}
		for _, k := range keys {
			if mr.Exists(k) {
				t.Fatalf("Reset 后分片键 %s 残留（pipeline DEL 未生效）", k)
			}
		}
		after := bfGatePtrs(bf)
		for i := range after {
			if after[i] == nil {
				t.Fatalf("复位后闸门 %d 为 nil（atomic.Pointer 零值陷阱）", i)
			}
			if after[i] == before[i] {
				t.Fatalf("闸门 %d 未换代（Reset 未重新武装 BF.RESERVE 闸门）", i)
			}
		}

		// 重新武装验证：换代后 reserveShard 必须再次实际执行 BF.RESERVE
		// （若闸门未换代，Do 因 once 燃尽直接跳过 f、返回 nil——错误
		// 重现即证明命令真实重发）。
		if err := bf.reserveShard(ctx, 0); err == nil {
			t.Fatal("Reset 后 reserveShard 未重新执行 BF.RESERVE（闸门复位失效）")
		}
	})
}

// TestBloomResetIdempotent 验证 Reset 幂等语义（对齐 Redis DEL）：对从未
// 写入的键 Reset 成功；连续两次 Reset 均 nil。bitmap 与 bf 两路径、单键与
// 分片形态都覆盖。
func TestBloomResetIdempotent(t *testing.T) {
	ctx := context.Background()

	rc, _ := newMiniRedisClient(t)
	cfg := defaultBloomConfig()
	cfg.policy = FailOpen
	b := newBitmapImpl(rc, "rst:idem", cfg)
	if err := b.Reset(ctx); err != nil {
		t.Fatalf("从未写入的单键 Reset：%v", err)
	}
	if err := b.Reset(ctx); err != nil {
		t.Fatalf("单键连续第二次 Reset：%v", err)
	}

	bs, _, _ := newShardedBitmapForTest(t, "rst:idem-s", FailOpen)
	if err := bs.Reset(ctx); err != nil {
		t.Fatalf("从未写入的分片 Reset：%v", err)
	}
	if err := bs.Reset(ctx); err != nil {
		t.Fatalf("分片连续第二次 Reset：%v", err)
	}
	if _, err := bs.Add(ctx, "after"); err != nil {
		t.Fatalf("双 Reset 后写入应正常：%v", err)
	}

	bf, _, _ := newShardedBFOnMini(t, "rst:idem-bf", FailOpen)
	if err := bf.Reset(ctx); err != nil {
		t.Fatalf("bf 从未写入的分片 Reset：%v", err)
	}
	if err := bf.Reset(ctx); err != nil {
		t.Fatalf("bf 分片连续第二次 Reset：%v", err)
	}
}

// TestBloomResetUnavailable 验证失败语义（规格硬约束）：Redis 不可用时
// Reset 返回错误且 errors.Is(ErrRedisUnavailable) 可感知；与 FailPolicy
// 取值无关——FailOpen 同样返回错误（不走放行兜底）。bitmap 分片（pipeline
// DEL）与 bf 分片两形态都覆盖。
func TestBloomResetUnavailable(t *testing.T) {
	ctx := context.Background()

	for _, policy := range []FailPolicy{FailOpen, FailClosed} {
		b, _, mr := newShardedBitmapForTest(t, "rst:uf-bmp", policy)
		if _, err := b.Add(ctx, "pre"); err != nil {
			t.Fatal(err)
		}
		mr.Close()
		if err := b.Reset(ctx); !errors.Is(err, ErrRedisUnavailable) {
			t.Fatalf("bitmap policy=%v 服务不可用 Reset 应返回哨兵错误，got %v", policy, err)
		}

		bf, _, mr2 := newShardedBFOnMini(t, "rst:uf-bf", policy)
		mr2.Close()
		if err := bf.Reset(ctx); !errors.Is(err, ErrRedisUnavailable) {
			t.Fatalf("bf policy=%v 服务不可用 Reset 应返回哨兵错误，got %v", policy, err)
		}
	}
}

// TestBloomResetRaceSmoke -race 冒烟：Reset 与 Add / reserveShard 并发轰炸。
// bitmap 侧压 DEL 与 Lua 置位的交错；bf 侧压新代码路径——reserveShard 的
// atomic.Pointer Load 与 Reset 的 Store、以及 sync.Once Do 与换代并发。
// miniredis 无 BF 模块，bfCmdImpl 侧命令报错属预期，只断言无 panic、
// 无数据竞争、错误不越界（nil 或 ErrRedisUnavailable）。
func TestBloomResetRaceSmoke(t *testing.T) {
	ctx := context.Background()

	t.Run("bitmap 单键 Reset×Add", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		cfg := defaultBloomConfig()
		cfg.capacity = 5000
		cfg.policy = FailOpen
		b := newBitmapImpl(rc, "rst:race", cfg)

		var wg sync.WaitGroup
		for i := range 16 {
			wg.Go(func() {
				for j := range 12 {
					_, err := b.Add(ctx, fmt.Sprintf("r-%d-%d", i, j))
					if err != nil && !errors.Is(err, ErrRedisUnavailable) {
						t.Errorf("并发 Add：%v", err)
						return
					}
				}
			})
		}
		for range 4 {
			wg.Go(func() {
				for range 8 {
					if err := b.Reset(ctx); err != nil && !errors.Is(err, ErrRedisUnavailable) {
						t.Errorf("并发 Reset：%v", err)
						return
					}
				}
			})
		}
		wg.Wait()
	})

	t.Run("bf 分片 Reset×reserveShard 闸门并发", func(t *testing.T) {
		bf, _, _ := newShardedBFOnMini(t, "rst:race-bf", FailOpen)
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Go(func() {
				for j := range 12 {
					// miniredis 无 BF 模块，bfCmdImpl.Add 报 unknown command
					// （数据类、预期内）；本冒烟只压闸门 Load/Store/Do 与
					// DEL 的并发安全——断言无 panic，-race 干净即可。
					_, _ = bf.Add(ctx, fmt.Sprintf("r-%d-%d", i, j))
				}
			})
		}
		for range 4 {
			wg.Go(func() {
				for range 8 {
					if err := bf.Reset(ctx); err != nil && !errors.Is(err, ErrRedisUnavailable) {
						t.Errorf("并发 bf.Reset：%v", err)
						return
					}
				}
			})
		}
		wg.Wait()
	})
}

// TestBloomResetBFCapacityRealRedisBloom 真 RedisBloom 环境下的 BF 路径
// 端到端回归（规格测试 3 关键项）：分片态 Reset 后再 Add，惰性闸门必须
// 以 perShard 容量重新 BF.RESERVE——断言分片键 BF.INFO 的 Capacity ==
// perShard 而非 RedisBloom 默认 100；并验证第二、三次 Add 不重复 RESERVE
// （闸门 Once 指针换代后保持稳定，同一把 Once 只消耗一次）。
//
// 环境需求：REDIS_CLUSTER（真实 Redis Cluster 且节点加载 bf 模块）——
// 分片 enabled 仅 ModeCluster 生效，standalone 无法构造 RESERVE 闸门链路；
// 本包既有测试体系（miniredis）不支持 RedisBloom 模块，无法驱动该路径，
// 故本用例在缺少环境时 skip（不引入新环境依赖，与 cluster_integration_test.go
// 同一环境约定，但不改那个文件）。
func TestBloomResetBFCapacityRealRedisBloom(t *testing.T) {
	raw := os.Getenv("REDIS_CLUSTER")
	if raw == "" {
		t.Skip("REDIS_CLUSTER 未设置：BF 闸门复位回归需真实 Redis Cluster + RedisBloom 模块")
	}
	rdb, err := NewWithUrl(raw)
	if err != nil {
		t.Fatalf("NewWithUrl: %v", err)
	}
	defer func() { _ = rdb.GracefulClose(context.Background()) }()
	if !rdb.Capability().HasModule("bf") {
		t.Skip("集群未加载 bf 模块（RedisBloom），跳过 BF 闸门复位回归")
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}

	ctx := context.Background()
	const (
		totalCap = 10000
		shardN   = 4
	)
	perShard := int64(totalCap / shardN) // 2500（须 ≥ minShardCapacity 才不被收缩）
	keyBF := bloomTestKey("bfreset-cluster")
	keySolo := bloomTestKey("bfreset-solo")
	defer func() {
		for _, k := range newBloomSharder(keyBF, true, shardN).allKeys() {
			_ = rc.Del(ctx, k).Err()
		}
		_ = rc.Del(ctx, keySolo).Err()
	}()

	bfCfg := defaultBloomConfig()
	WithCapacity(totalCap)(&bfCfg)
	WithFalsePositive(0.01)(&bfCfg)
	WithShardCount(shardN)(&bfCfg)
	bfCfg.policy = FailOpen // 工厂原默认赋值，直构需显式补
	bf := rc.newBFImpl(keyBF, bfCfg)
	if !bf.sharder.enabled || bf.sharder.n != shardN {
		t.Fatalf("集群环境分片未激活：enabled=%v n=%d（检查 REDIS_CLUSTER 是否为真集群）",
			bf.sharder.enabled, bf.sharder.n)
	}
	if bf.perShard != perShard {
		t.Fatalf("perShard got %d want %d", bf.perShard, perShard)
	}

	// shard0Info 直查分片 0 物理键的 BF.INFO（绕开聚合，断言单键容量）。
	shard0Info := func(what string) goredis.BFInfo {
		t.Helper()
		info, err := rc.BFInfo(ctx, bf.sharder.shardKey(0)).Result()
		if err != nil {
			t.Fatalf("%s：分片 0 键 BF.INFO：%v", what, err)
		}
		return info
	}
	// probeItem 返回路由到分片 0 的测试 item（只读探测，不落其他分片）。
	probeItem := func(tag string) any {
		t.Helper()
		for i := range 500 {
			item := fmt.Sprintf("%s-%s-%d", bloomTestKey(tag), "p", i)
			data, err := marshalItem(item)
			if err != nil {
				t.Fatal(err)
			}
			if bf.sharder.indexOf(data) == 0 {
				return item
			}
		}
		t.Fatalf("500 次采样未命中分片 0")
		return nil
	}

	// 全分片灌入并确认聚合容量。
	items := make([]any, 400)
	for i := range items {
		items[i] = fmt.Sprintf("bfr-%d", i)
	}
	if _, err := bf.AddMulti(ctx, items...); err != nil {
		t.Fatalf("分片 AddMulti：%v", err)
	}
	if got := shard0Info("首灌").Capacity; got != perShard {
		t.Fatalf("首灌后分片 0 Capacity got %d want %d（RESERVE 未生效？）", got, perShard)
	}
	gateBefore := bf.reserves[0].Load()

	// Reset 清空并确认物理键删除。
	if err := bf.Reset(ctx); err != nil {
		t.Fatalf("集群分片 Reset：%v", err)
	}
	if n, err := rc.Exists(ctx, bf.sharder.shardKey(0)).Result(); err != nil || n != 0 {
		t.Fatalf("Reset 后分片 0 键仍存在：n=%d err=%v", n, err)
	}

	// 复位验证第一步：Reset 后只读不重发 RESERVE（惰性维持）。
	probe := probeItem("rst")
	if ok, err := bf.Exists(ctx, probe); err != nil || ok {
		t.Fatalf("Reset 后 Exists(%v)：ok=%v err=%v（键应不存在）", probe, ok, err)
	}
	if _, err := rc.BFInfo(ctx, bf.sharder.shardKey(0)).Result(); err == nil {
		t.Fatal("Exists 路径不得重新 RESERVE（BF.INFO 应报键不存在）")
	}

	// 复位验证第二步：再 Add 必须以 perShard 重新 RESERVE。若闸门复位
	// 缺失（回归目标），键会被 RedisBloom 以默认 capacity=100 自动重建。
	if _, err := bf.Add(ctx, probe); err != nil {
		t.Fatalf("Reset 后 Add：%v", err)
	}
	gateRewarm := bf.reserves[0].Load()
	if gateRewarm == gateBefore {
		t.Fatal("Reset 后闸门 Once 未换代（复位未生效）")
	}
	if got := shard0Info("Reset 后重写").Capacity; got != perShard {
		t.Fatalf("Reset 后分片 0 Capacity got %d want %d（疑似默认 100 重建，闸门复位失效）", got, perShard)
	}

	// 复位验证第三步：第二、三次 Add 不重复 RESERVE——同一把换代后的
	// Once 只消耗一次，后续 Add 走直通路径（指针不变即闸门未再武装、
	// 也未再发 RESERVE；若错误地每次都 RESERVE，对已存在键会命中
	// "already exists" 吞错，行为无痕，指针换代是唯一白盒信号）。
	for _, item := range []any{probe, probeItem("rst2")} {
		if _, err := bf.Add(ctx, item); err != nil {
			t.Fatalf("后续 Add(%v)：%v", item, err)
		}
	}
	if p := bf.reserves[0].Load(); p != gateRewarm {
		t.Fatalf("第二三次 Add 不应重新武装闸门：%p != %p", p, gateRewarm)
	}
	if got := shard0Info("终检").Capacity; got != perShard {
		t.Fatalf("终检分片 0 Capacity got %d want %d", got, perShard)
	}
	info, err := bf.Info(ctx)
	if err != nil || info.NumItems < 1 {
		t.Fatalf("Reset 后重写 Info 异常：got %+v err=%v", info, err)
	}

	// 配套（规格测试 4 standalone 半句）：standalone BF 空键 Info 维持
	// 历史报错行为（emptyShardOK=false），Reset 不改动该路径。
	soloCfg := defaultBloomConfig()
	soloCfg.policy = FailOpen
	solo := rc.newBFImpl(keySolo, soloCfg)
	if err := solo.Reset(ctx); err != nil {
		t.Fatalf("standalone BF 对未写入键 Reset：%v", err)
	}
	if _, err := solo.Info(ctx); err == nil {
		t.Fatal("standalone 空过滤器 Info 应报 not found（既有行为不得回归）")
	}
}

// --- Card（去重基数估计） ---

// TestBloomCardBitmapStandalone 验证 bitmap 单键 Card：空键返回 0；Add 3
// 个不同 item 后 Card≈3（±1）；**同一 item 重复 Add 1000 次 Card 不增长**
// ——去重口径锚（位图状态不变则估计不变；对照 BF.* 路径 NumItems 的
// 插入口径会持续增长）。
func TestBloomCardBitmapStandalone(t *testing.T) {
	ctx := context.Background()
	rc, _ := newMiniRedisClient(t)
	cfg := defaultBloomConfig()
	cfg.capacity = 10000
	cfg.falsePositive = 0.01
	cfg.policy = FailOpen
	b := newBitmapImpl(rc, "card:solo", cfg)

	// 空键 → 0（BITCOUNT 对不存在键天然 0，非错误）
	n, err := b.Card(ctx)
	if err != nil || n != 0 {
		t.Fatalf("空过滤器 Card：got %d err=%v want 0,nil", n, err)
	}

	if _, err := b.AddMulti(ctx, "c1", "c2", "c3"); err != nil {
		t.Fatal(err)
	}
	n, err = b.Card(ctx)
	if err != nil {
		t.Fatalf("Card：%v", err)
	}
	if n < 2 || n > 4 {
		t.Fatalf("3 个不同 item 的 Card 应在 [2,4]（±1 容差），got %d", n)
	}

	// 重复插入不增长去重基数（位图状态不变 → 估计严格不变）
	before := n
	for range 1000 {
		if _, err := b.Add(ctx, "c1"); err != nil {
			t.Fatal(err)
		}
	}
	n, err = b.Card(ctx)
	if err != nil {
		t.Fatalf("重复 Add 后 Card：%v", err)
	}
	if n != before {
		t.Fatalf("同一 item 重复 Add 1000 次后 Card 增长：%d → %d（去重口径破坏）", before, n)
	}
}

// TestBloomCardBitmapSharded 验证 bitmap 分片态 Card 为全分片求和口径：
// 与逐分片独立估算之和一致（禁止合并 bitsSet 再估算的实现回归锚——每分片
// 单独 BITCOUNT + estimateNumItems），Reset 后归零。
func TestBloomCardBitmapSharded(t *testing.T) {
	ctx := context.Background()
	b, rc, _ := newShardedBitmapForTest(t, "card:shard", FailOpen)

	items := make([]any, 120)
	for i := range items {
		items[i] = fmt.Sprintf("card-s-%d", i)
	}
	if _, err := b.AddMulti(ctx, items...); err != nil {
		t.Fatal(err)
	}
	got, err := b.Card(ctx)
	if err != nil {
		t.Fatalf("分片 Card：%v", err)
	}
	if got <= 0 {
		t.Fatalf("灌入 120 item 后分片 Card 应 >0，got %d", got)
	}
	// 交叉锚：Card 必须等于逐分片独立估算的求和（口径可复算）
	var want int64
	for _, key := range b.sharder.allKeys() {
		bitsSet, err := rc.BitCount(ctx, key, nil).Result()
		if err != nil {
			t.Fatal(err)
		}
		want += b.estimateNumItems(bitsSet)
	}
	if got != want {
		t.Fatalf("分片 Card got %d want %d（逐分片独立估算求和口径）", got, want)
	}

	if err := b.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Card(ctx); err != nil || got != 0 {
		t.Fatalf("Reset 后分片 Card：got %d err=%v want 0,nil", got, err)
	}
}

// TestBloomCardUnavailable 验证服务不可用时 Card 返回 (0, 哨兵错误)：
// errors.Is(ErrRedisUnavailable) 可感知，且**不随 FailPolicy 分叉**——
// FailOpen 同样返回 (0, err)，观测类方法无"放行"概念。bitmap 分片与 bf
// 分片两路径都覆盖。
func TestBloomCardUnavailable(t *testing.T) {
	ctx := context.Background()

	for _, policy := range []FailPolicy{FailOpen, FailClosed} {
		b, _, mr := newShardedBitmapForTest(t, "card:uf-bmp", policy)
		mr.Close()
		if n, err := b.Card(ctx); n != 0 || !errors.Is(err, ErrRedisUnavailable) {
			t.Fatalf("bitmap policy=%v 不可用 Card：got (%d, %v)，want (0, ErrRedisUnavailable)", policy, n, err)
		}

		bf, _, mr2 := newShardedBFOnMini(t, "card:uf-bf", policy)
		mr2.Close()
		if n, err := bf.Card(ctx); n != 0 || !errors.Is(err, ErrRedisUnavailable) {
			t.Fatalf("bf policy=%v 不可用 Card：got (%d, %v)，want (0, ErrRedisUnavailable)", policy, n, err)
		}
	}
}

// TestBloomReserveGateDisarm 回归闸门失败解除武装（reserveShard 修复）：
// BF.RESERVE 报非 "exists" 类错误（miniredis 无 BF 模块，unknown command
// 即天然的失败注入）后，闸门必须**立即换代**（sync.Once 不辨成败，Do 返回
// 即燃尽——不显式 Store 新 Once 则服务恢复后永不再试，分片被默认
// capacity=100 隐式创建，容量契约静默作废）；下一次 reserveShard 必须
// 再次真实执行 BF.RESERVE（以"错误再现"为可观测信号：修复前 once 燃尽、
// Do 跳过闭包、返回 nil）。
//
// "exists" 类吞错分支（武装保持）无法在 miniredis 注入（需要真实 BF 响应），
// 由真 RedisBloom 环境的 TestBloomResetBFCapacityRealRedisBloom 覆盖：首灌
// RESERVE 成功后闸门 once 正常消耗、后续 Add 不再重发 RESERVE（指针稳定），
// 即"成功不解除武装"的同一不变量。
func TestBloomReserveGateDisarm(t *testing.T) {
	ctx := context.Background()
	bf, _, _ := newShardedBFOnMini(t, "gate:disarm", FailOpen)

	for round := range 3 {
		before := bf.reserves[0].Load()
		err := bf.reserveShard(ctx, 0)
		if err == nil {
			t.Fatalf("第 %d 轮：无 BF 模块环境 reserveShard 应报 unknown command", round+1)
		}
		if errors.Is(err, ErrRedisUnavailable) {
			t.Fatalf("第 %d 轮：unknown command 是数据类错误，不得走兜底哨兵：%v", round+1, err)
		}
		after := bf.reserves[0].Load()
		if after == before {
			t.Fatalf("第 %d 轮：RESERVE 真实失败后闸门未解除武装（Once 不辨成败，须显式 Store 新 Once）", round+1)
		}
		if after == nil {
			t.Fatalf("第 %d 轮：解除武装后的闸门为 nil（atomic.Pointer 零值陷阱）", round+1)
		}
	}

	// Add 路径联动：闸门每次失败后换代，bf.Add 反复尝试 RESERVE——
	// 错误持续可见即"服务恢复后即可自愈"的行为面（miniredis 永远无
	// BF 模块，修复前第二次 Add 会跳过 RESERVE 直发 BF.ADD 同样报错，
	// 无法区分；指针断言与 reserveShard 循环已覆盖判据）。
	for i := range 3 {
		if _, err := bf.Add(ctx, fmt.Sprintf("disarm-%d", i)); err == nil {
			t.Fatal("无 BF 模块环境 bf.Add 应报错")
		}
	}
}

// TestBloomCardResetCoexist Card 与 Reset 共存冒烟（bitmap 路径，miniredis
// 全环境可执行）：Add → Card>0 → Reset → Card==0；重写后 Card 回升。
func TestBloomCardResetCoexist(t *testing.T) {
	ctx := context.Background()
	rc, _ := newMiniRedisClient(t)
	cfg := defaultBloomConfig()
	cfg.capacity = 5000
	cfg.policy = FailOpen
	b := newBitmapImpl(rc, "card:coexist", cfg)

	if _, err := b.AddMulti(ctx, "x1", "x2", "x3", "x4"); err != nil {
		t.Fatal(err)
	}
	n, err := b.Card(ctx)
	if err != nil || n <= 0 {
		t.Fatalf("Add 后 Card：got %d err=%v want >0", n, err)
	}
	if err := b.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := b.Card(ctx); err != nil || n != 0 {
		t.Fatalf("Reset 后 Card：got %d err=%v want 0,nil", n, err)
	}
	if _, err := b.Add(ctx, "y1"); err != nil {
		t.Fatal(err)
	}
	if n, err := b.Card(ctx); err != nil || n <= 0 {
		t.Fatalf("重写后 Card 应回升：got %d err=%v", n, err)
	}
}

// TestBloomCardBFRealRedisBloom BF.* 路径 Card 的真环境回归（沿用包内
// RedisBloom 环境守卫手法，缺环境 skip 不引入新依赖）：
//   - standalone（REDIS_URL + bf 模块）：不存在键 → 0（BF.CARD 对空键
//     天然返回 0 不报错，与 BF.INFO 的 not-found 报错不同）；Add 后 >0；
//     Reset 后 → 0。
//   - 分片聚合（REDIS_CLUSTER + bf 模块）：Card == 逐分片 BF.CARD 直查
//     求和；Reset 后全分片 → 0。
func TestBloomCardBFRealRedisBloom(t *testing.T) {
	ctx := context.Background()

	t.Run("standalone", func(t *testing.T) {
		url := os.Getenv("REDIS_URL")
		if url == "" {
			t.Skip("REDIS_URL 未设置：跳过 BF.* Card standalone 回归（需 RedisBloom 环境）")
		}
		rdb, err := NewWithUrl(url)
		if err != nil {
			t.Fatalf("NewWithUrl: %v", err)
		}
		defer func() { _ = rdb.GracefulClose(context.Background()) }()
		if !rdb.Capability().HasModule("bf") {
			t.Skip("服务器未加载 bf 模块，跳过 BF.* Card standalone 回归")
		}

		key := bloomTestKey("card-bf")
		defer func() { _ = rdb.Del(ctx, key).Err() }()
		rc, ok := rdb.(*redisClient)
		if !ok {
			t.Fatalf("unexpected client type %T", rdb)
		}
		cfg := defaultBloomConfig()
		WithCapacity(10000)(&cfg)
		WithFalsePositive(0.01)(&cfg)
		cfg.policy = FailOpen // 工厂原默认赋值，直构需显式补
		bf := rc.newBFImpl(key, cfg)

		// 不存在键 → 0（不报错；若环境 RedisBloom 版本对空键报错，
		// 此断言即暴露口径差异）
		if n, err := bf.Card(ctx); err != nil || n != 0 {
			t.Fatalf("空键 BF.CARD：got (%d, %v) want (0, nil)", n, err)
		}
		items := make([]any, 20)
		for i := range items {
			items[i] = fmt.Sprintf("%s-cb-%d", key, i)
		}
		if _, err := bf.AddMulti(ctx, items...); err != nil {
			t.Fatalf("BF AddMulti：%v", err)
		}
		if n, err := bf.Card(ctx); err != nil || n <= 0 {
			t.Fatalf("Add 后 Card：got (%d, %v) want >0", n, err)
		}
		if err := bf.Reset(ctx); err != nil {
			t.Fatalf("BF Reset：%v", err)
		}
		if n, err := bf.Card(ctx); err != nil || n != 0 {
			t.Fatalf("Reset 后 Card：got (%d, %v) want (0, nil)", n, err)
		}
	})

	t.Run("分片聚合", func(t *testing.T) {
		raw := os.Getenv("REDIS_CLUSTER")
		if raw == "" {
			t.Skip("REDIS_CLUSTER 未设置：跳过 BF.* Card 分片聚合回归（需 RedisBloom 集群）")
		}
		rdb, err := NewWithUrl(raw)
		if err != nil {
			t.Fatalf("NewWithUrl: %v", err)
		}
		defer func() { _ = rdb.GracefulClose(context.Background()) }()
		if !rdb.Capability().HasModule("bf") {
			t.Skip("集群未加载 bf 模块，跳过 BF.* Card 分片聚合回归")
		}
		rc, ok := rdb.(*redisClient)
		if !ok {
			t.Fatalf("unexpected client type %T", rdb)
		}

		key := bloomTestKey("card-bf-s")
		scfg := defaultBloomConfig()
		WithCapacity(10000)(&scfg)
		WithFalsePositive(0.01)(&scfg)
		WithShardCount(4)(&scfg)
		scfg.policy = FailOpen
		bf := rc.newBFImpl(key, scfg)
		if !bf.sharder.enabled || bf.sharder.n != 4 {
			t.Fatalf("集群分片未激活：enabled=%v n=%d", bf.sharder.enabled, bf.sharder.n)
		}
		defer func() {
			for _, k := range bf.sharder.allKeys() {
				_ = rc.Del(ctx, k).Err()
			}
		}()

		items := make([]any, 200)
		for i := range items {
			items[i] = fmt.Sprintf("%s-cs-%d", key, i)
		}
		if _, err := bf.AddMulti(ctx, items...); err != nil {
			t.Fatalf("分片 AddMulti：%v", err)
		}
		got, err := bf.Card(ctx)
		if err != nil {
			t.Fatalf("分片 Card：%v", err)
		}
		// 聚合口径交叉验证：逐分片 BF.CARD 直查求和
		var want int64
		for _, k := range bf.sharder.allKeys() {
			n, err := rc.BFCard(ctx, k).Result()
			if err != nil {
				t.Fatalf("分片 %s BF.CARD 直查：%v", k, err)
			}
			want += n
		}
		if got != want {
			t.Fatalf("分片 Card got %d want %d（全分片求和口径）", got, want)
		}
		if got <= 0 {
			t.Fatalf("灌入 200 item 后 Card 应 >0，got %d", got)
		}
		if err := bf.Reset(ctx); err != nil {
			t.Fatalf("分片 Reset：%v", err)
		}
		if got, err := bf.Card(ctx); err != nil || got != 0 {
			t.Fatalf("Reset 后分片 Card：got (%d, %v) want (0, nil)", got, err)
		}
	})
}

// --- 真实环境（单机 / 集群 RedisBloom）用例补全（第五阶段 任务 1） ---
//
// 设计约束：本组用例一律**白盒直构**（newBitmapImpl / newBFImpl）或
// auto 工厂分派——工厂只有探测分派一条路径，路径对照语义由直构实现
// （newBitmapImpl 不经工厂，带 bf 模块的环境上也恒走 bitmap 回退路径）。

// realStandaloneClient 自建真单机守卫：internal 测试不能 import
// redis/test 包（循环依赖），沿用 os.Getenv + NewWithUrl 既有手法
// （TestBitmapConcurrentAddRealRedis 同型）。REDIS_URL 未设置即 skip。
// 返回内部 client 与服务器 bf 模块可用性。
func realStandaloneClient(t *testing.T) (*redisClient, bool) {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL 未设置：跳过真单机 Redis 用例")
	}
	rdb, err := NewWithUrl(url)
	if err != nil {
		t.Fatalf("NewWithUrl: %v", err)
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}
	t.Cleanup(func() { _ = rc.GracefulClose(context.Background()) })
	return rc, rdb.Capability().HasModule("bf")
}

// realClusterClient 自建真集群守卫（REDIS_CLUSTER 完整 URL），同上手法。
func realClusterClient(t *testing.T) (*redisClient, bool) {
	t.Helper()
	raw := os.Getenv("REDIS_CLUSTER")
	if raw == "" {
		t.Skip("REDIS_CLUSTER 未设置：跳过真集群 Redis 用例")
	}
	rdb, err := NewWithUrl(raw)
	if err != nil {
		t.Fatalf("NewWithUrl: %v", err)
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}
	t.Cleanup(func() { _ = rc.GracefulClose(context.Background()) })
	return rc, rdb.Capability().HasModule("bf")
}

// TestBloomBFResetRealStandalone 真实单机 RedisBloom 上 BF.* 原生路径的
// Reset 全链路（缺口 1）：经 auto 工厂分派取门面并断言落 bfCmdImpl，
// Add→Card>0→Reset→Exists false→Card==0→重写判新增→Info 正常。
// 单机形态 enabled=false、无惰性 RESERVE 闸门（历史既有行为）——闸门
// 复位断言由集群用例 TestBloomResetBFCapacityRealRedisBloom 覆盖。
// 共享实例纪律：bloomtest 随机前缀键、收尾 Del，严禁 FLUSHDB。
func TestBloomBFResetRealStandalone(t *testing.T) {
	rc, hasBF := realStandaloneClient(t)
	if !hasBF {
		t.Skip("服务器未加载 bf 模块（auto 分派不到 bfCmdImpl），跳过 BF.* 单机 Reset")
	}
	ctx := context.Background()
	key := bloomTestKey("bf-rst-121")
	t.Cleanup(func() { _ = rc.Del(ctx, key).Err() })

	f := rc.NewBloomFilter(key, WithCapacity(10000), WithFalsePositive(0.01))
	bf, ok := f.(*bfCmdImpl)
	if !ok {
		t.Fatalf("带 bf 模块单机 auto 应分派 bfCmdImpl，got %T", f)
	}
	if bf.sharder.enabled {
		t.Fatal("standalone 不应启用分片")
	}

	items := []any{"bf121-a", "bf121-b", "bf121-c"}
	if _, err := bf.AddMulti(ctx, items...); err != nil {
		t.Fatalf("BF AddMulti：%v", err)
	}
	for _, item := range items {
		ex, err := bf.Exists(ctx, item)
		if err != nil || !ex {
			t.Fatalf("Add 后 Exists(%v)：ok=%v err=%v", item, ex, err)
		}
	}
	if n, err := bf.Card(ctx); err != nil || n <= 0 {
		t.Fatalf("Add 后 Card：got (%d, %v) want >0", n, err)
	}

	if err := bf.Reset(ctx); err != nil {
		t.Fatalf("BF 单机 Reset：%v", err)
	}
	if n, err := rc.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Fatalf("Reset 后物理键仍存在：n=%d err=%v", n, err)
	}
	for _, item := range items {
		ex, err := bf.Exists(ctx, item)
		if err != nil {
			t.Fatalf("Reset 后 Exists(%v)：%v", item, err)
		}
		if ex {
			t.Fatalf("Reset 后 Exists(%v) 必须 false（清空语义破坏）", item)
		}
	}
	if n, err := bf.Card(ctx); err != nil || n != 0 {
		t.Fatalf("Reset 后 Card：got (%d, %v) want (0, nil)", n, err)
	}

	// 重写判新增（autocreate 路径），Info 恢复可用
	added, err := bf.Add(ctx, "bf121-a")
	if err != nil || !added {
		t.Fatalf("Reset 后重写 Add：added=%v err=%v", added, err)
	}
	info, err := bf.Info(ctx)
	if err != nil || info.NumItems < 1 {
		t.Fatalf("重写后 Info：got %+v err=%v", info, err)
	}
}

// TestBloomBitmapResetCardRealStandalone 真实单机上 bitmap 降级路径的
// Reset + Card（缺口 2）：白盒 newBitmapImpl 直构（带 bf 模块的服务器也
// 恒走 bitmap——不经工厂分派）。单机形态清空/幂等/
// Card 归零 + Info 佐证（NumFilters 恒 0 为 bitmap 回退版特征）。
func TestBloomBitmapResetCardRealStandalone(t *testing.T) {
	rc, _ := realStandaloneClient(t)
	ctx := context.Background()
	key := bloomTestKey("bmp-rst-121")
	t.Cleanup(func() { _ = rc.Del(ctx, key).Err() })

	cfg := defaultBloomConfig()
	cfg.capacity = 100000
	cfg.falsePositive = 0.01
	cfg.policy = FailOpen
	b := newBitmapImpl(rc, key, cfg)
	if b.sharder.enabled {
		t.Fatal("standalone 直构不应启用分片")
	}

	// 空过滤器口径
	if n, err := b.Card(ctx); err != nil || n != 0 {
		t.Fatalf("空过滤器 Card：got (%d, %v) want (0, nil)", n, err)
	}

	items := make([]any, 50)
	for i := range items {
		items[i] = fmt.Sprintf("bmp121-%d", i)
	}
	if _, err := b.AddMulti(ctx, items...); err != nil {
		t.Fatalf("bitmap AddMulti：%v", err)
	}
	n, err := b.Card(ctx)
	if err != nil || n <= 0 {
		t.Fatalf("Add 后 Card：got (%d, %v) want >0", n, err)
	}
	info, err := b.Info(ctx)
	if err != nil {
		t.Fatalf("Info：%v", err)
	}
	if info.NumFilters != 0 {
		t.Fatalf("bitmap 回退版 NumFilters 应恒 0，got %d（路径分派异常？）", info.NumFilters)
	}
	if info.Size <= 0 {
		t.Fatalf("bitmap Info.Size 应 >0（位图字节数），got %d", info.Size)
	}

	// Reset 幂等：连续两次均 nil
	if err := b.Reset(ctx); err != nil {
		t.Fatalf("bitmap 单机 Reset：%v", err)
	}
	if err := b.Reset(ctx); err != nil {
		t.Fatalf("bitmap 单机二次 Reset（幂等）：%v", err)
	}
	if n, err := rc.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Fatalf("Reset 后物理键仍存在：n=%d err=%v", n, err)
	}
	if got, err := b.Card(ctx); err != nil || got != 0 {
		t.Fatalf("Reset 后 Card：got (%d, %v) want (0, nil)", got, err)
	}
	for _, item := range items[:10] {
		ex, err := b.Exists(ctx, item)
		if err != nil || ex {
			t.Fatalf("Reset 后 Exists(%v)：ok=%v err=%v", item, ex, err)
		}
	}
	added, err := b.Add(ctx, items[0])
	if err != nil || !added {
		t.Fatalf("Reset 后重写：added=%v err=%v", added, err)
	}
	if got, err := b.Card(ctx); err != nil || got <= 0 {
		t.Fatalf("重写后 Card 应回升：got (%d, %v)", got, err)
	}
}

// TestBloomBitmapResetCardRealCluster 真实集群上 bitmap 降级路径的
// Reset + Card（缺口 3）：单机键与 WithShardCount(8) 分片两形态。
// 集群上直构 newBitmapImpl 同样恒走 bitmap（无 bf 依赖）；分片形态
// 断言全部物理键清除（allKeys 口径）、Card/Info 归零、重写路由正确。
func TestBloomBitmapResetCardRealCluster(t *testing.T) {
	rc, _ := realClusterClient(t)
	if rc.Mode() != ModeCluster {
		t.Fatalf("REDIS_CLUSTER 连接的模式应为 ModeCluster，got %v", rc.Mode())
	}
	ctx := context.Background()

	t.Run("集群单键", func(t *testing.T) {
		key := bloomTestKey("bmpclu-solo")
		t.Cleanup(func() { _ = rc.Del(ctx, key).Err() })
		cfg := defaultBloomConfig()
		cfg.capacity = 100000
		cfg.policy = FailOpen
		b := newBitmapImpl(rc, key, cfg)
		if b.sharder.enabled {
			t.Fatal("集群未显式请求分片不应启用（opt-in 契约）")
		}
		if _, err := b.AddMulti(ctx, "cs1", "cs2", "cs3"); err != nil {
			t.Fatalf("集群单键 AddMulti：%v", err)
		}
		if n, err := b.Card(ctx); err != nil || n <= 0 {
			t.Fatalf("集群单键 Card：got (%d, %v)", n, err)
		}
		if err := b.Reset(ctx); err != nil {
			t.Fatalf("集群单键 Reset：%v", err)
		}
		if n, err := rc.Exists(ctx, key).Result(); err != nil || n != 0 {
			t.Fatalf("Reset 后裸 base 键仍存在：n=%d err=%v", n, err)
		}
		if got, err := b.Card(ctx); err != nil || got != 0 {
			t.Fatalf("Reset 后 Card：got (%d, %v)", got, err)
		}
		if _, err := b.Add(ctx, "cs1"); err != nil {
			t.Fatalf("重写：%v", err)
		}
	})

	t.Run("集群分片8", func(t *testing.T) {
		key := bloomTestKey("bmpclu-s8")
		cfg := defaultBloomConfig()
		cfg.capacity = 100_000 // effectiveN = min(8, 100000/1000) = 8，不被收缩
		cfg.shardCount = 8
		cfg.policy = FailOpen
		b := newBitmapImpl(rc, key, cfg)
		if !b.sharder.enabled || b.sharder.n != 8 {
			t.Fatalf("集群分片未激活/收缩异常：enabled=%v n=%d", b.sharder.enabled, b.sharder.n)
		}
		keys := b.sharder.allKeys()
		t.Cleanup(func() {
			for _, k := range keys {
				_ = rc.Del(ctx, k).Err()
			}
		})

		items := make([]any, 2000)
		for i := range items {
			items[i] = fmt.Sprintf("bmpclu-%d", i)
		}
		if _, err := b.AddMulti(ctx, items...); err != nil {
			t.Fatalf("分片 AddMulti：%v", err)
		}
		// 2000 个 item 应散布命中全部 8 个分片键
		for _, k := range keys {
			n, err := rc.Exists(ctx, k).Result()
			if err != nil || n != 1 {
				t.Fatalf("分片键 %s 应存在：n=%d err=%v", k, n, err)
			}
		}
		card, err := b.Card(ctx)
		if err != nil || card <= 0 {
			t.Fatalf("分片 Card：got (%d, %v)", card, err)
		}
		info, err := b.Info(ctx)
		if err != nil || info.NumItems <= 0 {
			t.Fatalf("分片 Info：got %+v err=%v", info, err)
		}

		if err := b.Reset(ctx); err != nil {
			t.Fatalf("分片 Reset：%v", err)
		}
		for _, k := range keys {
			n, err := rc.Exists(ctx, k).Result()
			if err != nil || n != 0 {
				t.Fatalf("Reset 后分片键 %s 残留：n=%d err=%v", k, n, err)
			}
		}
		if got, err := b.Card(ctx); err != nil || got != 0 {
			t.Fatalf("Reset 后分片 Card：got (%d, %v)", got, err)
		}
		info, err = b.Info(ctx)
		if err != nil || info.NumItems != 0 {
			t.Fatalf("Reset 后分片 Info.NumItems 应归零：got %+v err=%v", info, err)
		}

		// 重写路由正确：抽样断言落点与 sharder 口径一致
		for _, item := range items[:30] {
			if _, err := b.Add(ctx, item); err != nil {
				t.Fatalf("重写 Add(%v)：%v", item, err)
			}
			data, err := marshalItem(item)
			if err != nil {
				t.Fatal(err)
			}
			routed := b.sharder.keyFor(data)
			n, err := rc.Exists(ctx, routed).Result()
			if err != nil || n != 1 {
				t.Fatalf("重写未落路由分片键 %s：n=%d err=%v", routed, n, err)
			}
		}
		if got, err := b.Card(ctx); err != nil || got <= 0 {
			t.Fatalf("重写后 Card 应回升：got (%d, %v)", got, err)
		}
	})
}

// TestBloomScaleFPRRealStandalone 真单机双路径 10k 规模用例（缺口 4）：
// 1M 容量、FPR 预算 0.01——灌入 10k 唯一 item（AddMulti 分批 1000），
// 再对 10k 个**从未写入**的 item 批量查询（ExistsMulti 分批），实测
// 假阳率断言 ≤ 预算×2（sanity 量级，非精确统计承诺；10k/1M 远未饱和，
// 理论 FP 应显著低于预算）。记录两侧耗时供回归对比。
func TestBloomScaleFPRRealStandalone(t *testing.T) {
	rc, hasBF := realStandaloneClient(t)
	ctx := context.Background()
	const (
		capacity = 1_000_000
		budget   = 0.01
		n        = 10_000
		batch    = 1_000
	)

	runPath := func(name string, f BloomFilter, key string) {
		t.Helper()
		t.Cleanup(func() { _ = rc.Del(ctx, key).Err() })

		hits := make([]any, n)
		misses := make([]any, n)
		for i := range hits {
			hits[i] = fmt.Sprintf("scale:%s:hit:%d", name, i)
			misses[i] = fmt.Sprintf("scale:%s:miss:%d", name, i)
		}

		start := time.Now()
		for b := 0; b < n; b += batch {
			if _, err := f.AddMulti(ctx, hits[b:b+batch]...); err != nil {
				t.Fatalf("%s 灌入批次 %d：%v", name, b, err)
			}
		}
		insertDur := time.Since(start)

		var fp int
		start = time.Now()
		for b := 0; b < n; b += batch {
			res, err := f.ExistsMulti(ctx, misses[b:b+batch]...)
			if err != nil {
				t.Fatalf("%s 查询批次 %d：%v", name, b, err)
			}
			for _, v := range res {
				if v {
					fp++
				}
			}
		}
		queryDur := time.Since(start)

		// 写入侧自洽抽查：布隆无假阴性，抽样已灌入项必须 true
		sample, err := f.ExistsMulti(ctx, hits[:batch]...)
		if err != nil {
			t.Fatalf("%s 抽查：%v", name, err)
		}
		for i, v := range sample {
			if !v {
				t.Fatalf("%s 已灌入项 %v 假阴性（数据面破坏）", name, hits[i])
			}
		}

		t.Logf("%s 路径 %dk 规模：插入 %v（%.0f item/s），查询 %v，实测 FPR=%.4f（预算 %.2f，断言上限 %.2f）",
			name, n/1000, insertDur, float64(n)/insertDur.Seconds(), queryDur,
			float64(fp)/float64(n), budget, budget*2)
		if limit := int64(float64(n) * budget * 2); int64(fp) > limit {
			t.Fatalf("%s 实测 FPR %d/%d 超预算×2（上限 %d）——容量规划或哈希布局回归", name, fp, n, limit)
		}
	}

	if hasBF {
		// bf 路径经 auto 工厂分派（带模块即 bfCmdImpl）
		key := bloomTestKey("scale-bf")
		f := rc.NewBloomFilter(key, WithCapacity(capacity), WithFalsePositive(budget))
		if _, ok := f.(*bfCmdImpl); !ok {
			t.Fatalf("带 bf 模块 auto 应分派 bfCmdImpl，got %T", f)
		}
		runPath("bf", f, key)
	} else {
		t.Log("服务器无 bf 模块：跳过 BF 路径规模用例")
	}

	bmpKey := bloomTestKey("scale-bmp")
	cfg := defaultBloomConfig()
	cfg.capacity = capacity
	cfg.falsePositive = budget
	cfg.policy = FailOpen
	runPath("bitmap", newBitmapImpl(rc, bmpKey, cfg), bmpKey)
}

// TestBloomBitmapConcurrentRealStandalone 真单机 bitmap 路径并发冒烟
// （缺口 5）：50 worker（25 Add + 25 Exists）+ 4 个同键并发 Reset 轰炸。
// Reset 与写入无全序保证（接口注释明示），断言口径：错误只允许 nil 或
// ErrRedisUnavailable 哨兵、无 panic、无协议错乱类数据错误；并发收敛后
// 串行哨兵写入 + 存在性 + Card 确定性自检。
func TestBloomBitmapConcurrentRealStandalone(t *testing.T) {
	rc, _ := realStandaloneClient(t)
	ctx := context.Background()
	key := bloomTestKey("conc-rst")
	t.Cleanup(func() { _ = rc.Del(ctx, key).Err() })

	cfg := defaultBloomConfig()
	cfg.capacity = 100_000
	cfg.policy = FailOpen
	b := newBitmapImpl(rc, key, cfg)

	okErr := func(err error) bool {
		return err == nil || errors.Is(err, ErrRedisUnavailable)
	}

	var wg sync.WaitGroup
	for i := range 25 { // Add worker：每轮唯一 item
		wg.Go(func() {
			for j := range 20 {
				if _, err := b.Add(ctx, fmt.Sprintf("cw-%d-%d", i, j)); !okErr(err) {
					t.Errorf("并发 Add：%v", err)
					return
				}
			}
		})
	}
	for range 25 { // Exists worker：查共享池（世代交错下 true/false 都合法）
		wg.Go(func() {
			for j := range 20 {
				if _, err := b.Exists(ctx, fmt.Sprintf("cw-%d-%d", j%25, j)); !okErr(err) {
					t.Errorf("并发 Exists：%v", err)
					return
				}
			}
		})
	}
	for range 4 { // Reset 轰炸
		wg.Go(func() {
			for range 30 {
				if err := b.Reset(ctx); !okErr(err) {
					t.Errorf("并发 Reset：%v", err)
					return
				}
			}
		})
	}
	wg.Wait()

	// 收敛后串行自检（无并发干扰，行为必须确定）
	added, err := b.Add(ctx, "serial-sentinel")
	if err != nil || !added {
		t.Fatalf("串行哨兵 Add：added=%v err=%v", added, err)
	}
	ex, err := b.Exists(ctx, "serial-sentinel")
	if err != nil || !ex {
		t.Fatalf("串行哨兵 Exists：ok=%v err=%v", ex, err)
	}
	if n, err := b.Card(ctx); err != nil || n <= 0 {
		t.Fatalf("串行哨兵后 Card：got (%d, %v) want >0", n, err)
	}
}

// TestBloomReserveAlreadyExistsSwallowRealCluster 真集群覆盖
// reserveShard 的 "already exists" 吞错分支（评审 P3）：同一分片 base 键
// 上构造两个 bf 实例——实例 1 先写入完成全分片惰性 BF.RESERVE，实例 2 的
// 每个分片首次写入触发的 BF.RESERVE 必然命中"过滤器已存在"。断言：
//  1. 实例 2 Add 成功不报错（吞错生效；若错误措辞不匹配吞错子串，Redis
//     报错会原样上抛，本断言即暴露）；
//  2. 实例 2 各分片闸门 Once 指针在写入前后不变——命中 exists 类视为
//     成功、**维持武装**（区别于真实失败路径的解除武装换代，见
//     TestBloomReserveGateDisarm）；
//  3. 探针：各分片 BF.INFO 的 Capacity 恒等于实例 1 的配置口径
//     perShard（第二实例的重复 RESERVE 尝试未破坏既有预分配）；
//  4. 双实例读写互通（同物理键）：实例 2 可读实例 1 的 seed、实例 1 可
//     读实例 2 的 probe。
//
// 环境守卫：REDIS_CLUSTER + 集群 bf 模块，缺则 skip。
func TestBloomReserveAlreadyExistsSwallowRealCluster(t *testing.T) {
	raw := os.Getenv("REDIS_CLUSTER")
	if raw == "" {
		t.Skip("REDIS_CLUSTER 未设置：跳过 BF.RESERVE already-exists 吞错真机回归")
	}
	rdb, err := NewWithUrl(raw)
	if err != nil {
		t.Fatalf("NewWithUrl: %v", err)
	}
	defer func() { _ = rdb.GracefulClose(context.Background()) }()
	if !rdb.Capability().HasModule("bf") {
		t.Skip("集群未加载 bf 模块，跳过 BF.RESERVE already-exists 吞错真机回归")
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}

	ctx := context.Background()
	const (
		shardN   = 8
		totalCap = 100_000
	)
	perShardWant := int64(totalCap / shardN) // 12500，无收缩（≥ minShardCapacity）

	base := bloomTestKey("bfexists-sw")
	mk := func() *bfCmdImpl {
		cfg := defaultBloomConfig()
		WithCapacity(totalCap)(&cfg)
		WithFalsePositive(0.01)(&cfg)
		WithShardCount(shardN)(&cfg)
		cfg.policy = FailOpen
		return rc.newBFImpl(base, cfg)
	}
	bf1, bf2 := mk(), mk()
	if !bf1.sharder.enabled || bf1.sharder.n != shardN {
		t.Fatalf("实例 1 分片未激活：enabled=%v n=%d", bf1.sharder.enabled, bf1.sharder.n)
	}
	if bf1.perShard != perShardWant || bf2.perShard != perShardWant {
		t.Fatalf("perShard got %d/%d want %d", bf1.perShard, bf2.perShard, perShardWant)
	}
	keys := bf1.sharder.allKeys()
	t.Cleanup(func() {
		for _, k := range keys { // 逐键 Del（多键跨 slot 会 CROSSSLOT）
			_ = rc.Del(ctx, k).Err()
		}
	})

	// 基线：实例 1 灌 300 项（8 分片全命中概率 ~1-(7/8)^300 ≈ 1），
	// 各分片惰性 RESERVE 以 perShard 建键，远未触发扩容。
	seed := make([]any, 300)
	for i := range seed {
		seed[i] = fmt.Sprintf("%s-seed-%d", base, i)
	}
	if _, err := bf1.AddMulti(ctx, seed...); err != nil {
		t.Fatalf("实例 1 AddMulti：%v", err)
	}
	assertShardCapacity := func(what string) {
		t.Helper()
		for idx, k := range keys {
			info, err := rc.BFInfo(ctx, k).Result()
			if err != nil {
				t.Fatalf("%s：分片 %d（%s）BF.INFO：%v", what, idx, k, err)
			}
			if info.Capacity != perShardWant {
				t.Fatalf("%s：分片 %d Capacity got %d want %d（第二实例 RESERVE 破坏预分配？）",
					what, idx, info.Capacity, perShardWant)
			}
		}
	}
	assertShardCapacity("实例 1 灌入后基线")

	// 每个分片挑一个路由命中的实例 2 probe item，令 8 把闸门全部经历
	// "已存在键上的 BF.RESERVE"。
	probes := make([]any, shardN)
	for i := range probes {
		for j := range 500 {
			item := fmt.Sprintf("%s-pr-%d-%d", base, i, j)
			data, err := marshalItem(item)
			if err != nil {
				t.Fatal(err)
			}
			if bf2.sharder.indexOf(data) == i {
				probes[i] = item
				break
			}
		}
		if probes[i] == nil {
			t.Fatalf("500 次采样未命中分片 %d", i)
		}
	}

	gatesBefore := bfGatePtrs(bf2)
	for _, item := range probes {
		added, err := bf2.Add(ctx, item)
		if err != nil {
			t.Fatalf("实例 2 Add(%v)：RESERVE 命中 already exists 应被吞错维持成功语义，got err=%v（若为 exists 措辞漂移即生产缺陷候选）", item, err)
		}
		_ = added // 全新 probe，新增判定 true/false 均合法（假阳性事件）
	}
	gatesAfter := bfGatePtrs(bf2)
	for i := range gatesAfter {
		if gatesAfter[i] != gatesBefore[i] {
			t.Fatalf("分片 %d 闸门在 exists 吞错后解除武装（指针换代 %p→%p）——exists 类应视为成功、维持武装",
				i, gatesBefore[i], gatesAfter[i])
		}
	}

	// 探针复测：第二实例的 8 次 RESERVE 尝试未破坏各分片容量口径
	assertShardCapacity("实例 2 吞错写入后")

	// 双实例读写互通（同物理键、不同实例对象）
	for _, item := range seed[:50] {
		ex, err := bf2.Exists(ctx, item)
		if err != nil || !ex {
			t.Fatalf("实例 2 读实例 1 的 seed(%v)：ok=%v err=%v", item, ex, err)
		}
	}
	for _, item := range probes {
		ex, err := bf1.Exists(ctx, item)
		if err != nil || !ex {
			t.Fatalf("实例 1 读实例 2 的 probe(%v)：ok=%v err=%v", item, ex, err)
		}
	}
}
