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
	"testing"

	"github.com/alicebob/miniredis"
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
			for i := 0; i < 5000; i++ {
				item := fmt.Sprintf("parity-%d", i)
				sum := xxh3.Hash128([]byte(item))
				h1, h2 := sum.Hi, sum.Lo|1
				if h2%2 != 1 {
					t.Fatalf("步长 h2 必须恒奇，item=%s h2=%d", item, h2)
				}

				pos := b.hashs(item)
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
				for _, pos := range b.hashs(fmt.Sprintf("inserted-%d", i)) {
					bitset[pos] = true
				}
			}

			var fp int64
			var dupItems int64
			for i := int64(0); i < c.n; i++ {
				positions := b.hashs(fmt.Sprintf("missing-%d", i))
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

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
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
		}()
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
		positions := b.hashs(item)
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
	for i := 0; i < n; i++ {
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
	for i := 0; i < n; i++ {
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
		positions := b.hashs(item)
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
	items := []string{"m1", "m2", "m3"}

	args := b.multiPositionsArgs(items)
	if len(args) != 1+len(items)*int(b.k) {
		t.Fatalf("参数总数 got %d want %d", len(args), 1+len(items)*int(b.k))
	}
	if kk, ok := args[0].(uint); !ok || kk != b.k {
		t.Fatalf("ARGV[0] 应为 k=%d，got %v", b.k, args[0])
	}
	for i, item := range items {
		want := b.hashs(item)
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
		multiArgs := []string{"u2", "u1", "u3"}
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
	items := []string{"x1", "x2", "x3", "x4", "x5", "x6", "x7"}

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
	again := []string{"x7", "nope", "x3"}
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
	query := []string{"nope", "x1", "zzz", "x7"}
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
			first := shardIndex(item, n)
			if first < 0 || first >= n {
				t.Fatalf("n=%d 值域越界：item=%s idx=%d", n, item, first)
			}
			for r := 0; r < 2; r++ {
				if got := shardIndex(item, n); got != first {
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
		if got := shardIndex(item, 1); got != 0 {
			t.Fatalf("n=1 应恒 0，got %d", got)
		}
		if got := shardIndex(item, 0); got != 0 {
			t.Fatalf("n=0 防御性应恒 0，got %d", got)
		}
	}

	// 均匀散布：8000 items × 8 桶
	const n, total = 8, 8000
	buckets := make([]int, n)
	for i := 0; i < total; i++ {
		buckets[shardIndex(fmt.Sprintf("uniform-%d", i), n)]++
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
		{"默认 8 片整容量", 8, 1_000_000, 8, 125_000},
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

// TestResolveBloomShardingModes 验证触发条件（规格 1）：仅 cluster 启用
// 分片；standalone/sentinel/ring 关闭且 perShard==总容量（键名行为零回归）。
func TestResolveBloomShardingModes(t *testing.T) {
	for _, mode := range []Mode{ModeStandalone, ModeSentinel, ModeRing} {
		enabled, n, per := resolveBloomSharding(mode, 8, 100_000)
		if enabled || n != 1 || per != 100_000 {
			t.Fatalf("%s 模式不应分片：enabled=%v n=%d per=%d", mode, enabled, n, per)
		}
	}
	enabled, n, per := resolveBloomSharding(ModeCluster, 8, 100_000)
	if !enabled || n != 8 || per != 12_500 {
		t.Fatalf("cluster 模式应 8 分片、每片 12500：enabled=%v n=%d per=%d", enabled, n, per)
	}
	// 集群退化态：effectiveN 收缩为 1 时 enabled 仍为 true（键名带 #0）
	enabled, n, _ = resolveBloomSharding(ModeCluster, 8, 1_500)
	if !enabled || n != 1 {
		t.Fatalf("cluster 退化态应 enabled=true n=1，got enabled=%v n=%d", enabled, n)
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
		if got := s.keyFor("anything"); got != "plain" {
			t.Fatalf("关闭态 keyFor 应为 base，got %q", got)
		}
		if keys := s.allKeys(); len(keys) != 1 || keys[0] != "plain" {
			t.Fatalf("关闭态 allKeys got %v", keys)
		}
		groups := s.group([]string{"a", "b", "c"})
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
		groups := s.group([]string{"x", "y"})
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
		for i := 0; i < 500; i++ {
			item := fmt.Sprintf("sk-%d", i)
			k := s.keyFor(item)
			seen[k] = true
			if want := "base#" + fmt.Sprint(shardIndex(item, 4)); k != want {
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
		groups := s.group(items)
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
				if shardIndex(item, 6) != g.idx {
					t.Fatalf("item %s 落错分组（idx=%d）", item, g.idx)
				}
				orig := g.srcIdx[j]
				if items[orig] != item {
					t.Fatalf("srcIdx[%d]=%d 指回错误 item：%q != %q", j, orig, items[orig], item)
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
// 默认 8；n<=0 静默忽略保留默认；任意正整数合法（含 1）。
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

	items := make([]string, 200)
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
		for i := 0; i < nShards; i++ {
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
			key := b.sharder.keyFor(item)
			for _, pos := range b.hashs(item) {
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
		mix := make([]string, 0, 60)
		for i := 0; i < 40; i++ {
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
	items := make([]string, 120)
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
	qa := []string{items[5], "lbc-nope-1", items[60]}
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
	qu := []string{"lbc-nope-1", items[0], items[7], "lbc-nope-2"}
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
	items := make([]string, 50)
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
		if bf.sharder.keyFor(item) != bm.sharder.keyFor(item) {
			t.Fatalf("两路径 keyFor(%s) 分叉：%q vs %q", item,
				bf.sharder.keyFor(item), bm.sharder.keyFor(item))
		}
		if bf.sharder.indexOf(item) != bm.sharder.indexOf(item) {
			t.Fatalf("两路径 indexOf(%s) 分叉", item)
		}
	}
	ga, gb := bf.sharder.group(items), bm.sharder.group(items)
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

// --- WithBloomImpl 强制实现路径（A/B 对照） ---

// TestBloomImplOptionSelection 验证 WithBloomImpl 在 miniredis（无 bf 模块）
// 上的分派与语义（规格 5a/5b）：
//   - 默认零值 BloomImplAuto：按 HasBloom() 探测 → bitmapImpl（既有行为）；
//   - 强制 bitmap：选定 bitmapImpl 且行为正常（四类方法自洽）；
//   - 强制 BF.*：选定 bfCmdImpl，服务器无模块时命令直接报错且**不降级**——
//     错误原样返回、不得命中 ErrRedisUnavailable 哨兵（unknown 类不是
//     Unavailable，不走 FailOpen 兜底）；
//   - 两种强制模式都不触发 HasBloom() 探测（capability 缓存保持未就绪）；
//   - 越界枚举值按 auto 处理（与包内"非法值回落默认"惯例一致）。
func TestBloomImplOptionSelection(t *testing.T) {
	ctx := context.Background()

	// 零值契约：默认配置必须是 auto
	if got := defaultBloomConfig().impl; got != BloomImplAuto {
		t.Fatalf("BloomImpl 默认应为零值 auto，got %d", got)
	}

	t.Run("auto 探测分派到 bitmap", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		f := rc.NewBloomFilter("sel:auto")
		if _, ok := f.(*bitmapImpl); !ok {
			t.Fatalf("miniredis 无 bf 模块，auto 应选 bitmapImpl，got %T", f)
		}
	})

	t.Run("强制 bitmap 行为正常且跳过探测", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		f := rc.NewBloomFilter("sel:bmp", WithBloomImpl(BloomImplBitmap))
		bm, ok := f.(*bitmapImpl)
		if !ok {
			t.Fatalf("强制 bitmap 应选 bitmapImpl，got %T", f)
		}
		if rc.cap.ready {
			t.Fatal("强制模式不得触发 HasBloom() 探测（capability 已就绪=发过 INFO）")
		}

		added, err := bm.Add(ctx, "s1")
		if err != nil || !added {
			t.Fatalf("强制 bitmap Add：added=%v err=%v", added, err)
		}
		res, err := bm.AddMulti(ctx, "s2", "s1", "s3")
		if err != nil {
			t.Fatalf("强制 bitmap AddMulti：%v", err)
		}
		if fmt.Sprint(res) != "[true false true]" {
			t.Fatalf("AddMulti 语义/顺序错误：got %v", res)
		}
		ex, err := bm.ExistsMulti(ctx, "s3", "s-missing")
		if err != nil || ex[0] != true {
			t.Fatalf("ExistsMulti：got %v err=%v", ex, err)
		}
		info, err := bm.Info(ctx)
		if err != nil || info.Capacity != 1_000_000 {
			t.Fatalf("Info：got %+v err=%v", info, err)
		}
	})

	t.Run("强制 BF 无模块报错不降级", func(t *testing.T) {
		rc, _ := newMiniRedisClient(t)
		f := rc.NewBloomFilter("sel:bf", WithBloomImpl(BloomImplBF))
		bf, ok := f.(*bfCmdImpl)
		if !ok {
			t.Fatalf("强制 BF 应选 bfCmdImpl，got %T", f)
		}
		if rc.cap.ready {
			t.Fatal("强制模式不得触发 HasBloom() 探测")
		}

		// Add/Exists/AddMulti/ExistsMulti/Info 全部直接报错（原样返回），
		// 不降级到 bitmap、不触发 FailOpen 兜底。错误文本不做断言——
		// miniredis 与真实 Redis 的 unknown 命令措辞不同。
		addCallers := map[string]func() error{
			"Add":         func() error { _, err := bf.Add(ctx, "x"); return err },
			"Exists":      func() error { _, err := bf.Exists(ctx, "x"); return err },
			"AddMulti":    func() error { _, err := bf.AddMulti(ctx, "x", "y"); return err },
			"ExistsMulti": func() error { _, err := bf.ExistsMulti(ctx, "x", "y"); return err },
			"Info":        func() error { _, err := bf.Info(ctx); return err },
		}
		for name, call := range addCallers {
			err := call()
			if err == nil {
				t.Fatalf("强制 BF 在无模块服务器上 %s 应报错（不自动降级），got nil", name)
			}
			if errors.Is(err, ErrRedisUnavailable) {
				t.Fatalf("强制 BF %s 的报错是数据类（unknown command 风格），不得走兜底哨兵，got %v", name, err)
			}
		}
	})

	t.Run("越界枚举按 auto 回落", func(t *testing.T) {
		cfg := defaultBloomConfig()
		WithBloomImpl(BloomImpl(99))(&cfg)
		if cfg.impl != BloomImpl(99) {
			t.Fatalf("option 应原样存储，分派层归一 auto；got %d", cfg.impl)
		}
		rc, _ := newMiniRedisClient(t)
		f := rc.NewBloomFilter("sel:bad", WithBloomImpl(BloomImpl(99)))
		if _, ok := f.(*bitmapImpl); !ok {
			t.Fatalf("越界值应按 auto 分派（miniredis → bitmapImpl），got %T", f)
		}
	})
}

// TestBloomImplABRealRedis 在真实 Redis 上用 WithBloomImpl 对 BF.* 与
// bitmap 两条路径做 A/B 对照（规格 5c）：两条独立 key、同容量同参数，
// 各跑 Add/AddMulti/Exists/ExistsMulti——每路径 1000 元素自洽（布隆无
// 假阴性，已灌入项必须全 true）+ Info 合理性断言。**不做跨路径的严格
// 对比断言**（BF 自动扩容 vs bitmap 固定布局、ItemsInserted 精确值 vs
// 估计值，逐项相等必 flaky）。
// 守卫：REDIS_URL 未设置或服务器无 bf 模块（RedisBloom 部署 / Redis 8.x
// community 内置）时跳过——无模块环境无法构造 BF 路径对照。
// 共享实例纪律：key 带 bloomtest:<随机> 前缀，收尾 Del，严禁 FLUSHDB。
func TestBloomImplABRealRedis(t *testing.T) {
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
	items := make([]string, n)
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
		for i := 0; i < n/2; i++ {
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

	bf := rdb.NewBloomFilter(keyBF,
		WithCapacity(10_000), WithFalsePositive(0.01),
		WithBloomImpl(BloomImplBF))
	bmp := rdb.NewBloomFilter(keyBMP,
		WithCapacity(10_000), WithFalsePositive(0.01),
		WithBloomImpl(BloomImplBitmap))

	// 分派正确性（真实 Redis 上 auto 反而会选 BF，这里必须按强制选项）
	if _, ok := bf.(*bfCmdImpl); !ok {
		t.Fatalf("强制 BF 应得 bfCmdImpl，got %T", bf)
	}
	if _, ok := bmp.(*bitmapImpl); !ok {
		t.Fatalf("强制 bitmap 应得 bitmapImpl，got %T", bmp)
	}

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
