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
//     先 exists 后补位、返回 !exists）。历史上 Lua 路径误用 all_zero（k 位
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
		positions := b.hashs(item)
		for _, pos := range positions {
			if err := rc.SetBit(ctx, b.key, int64(pos), 1).Err(); err != nil {
				t.Fatalf("预置 SetBit(%d,1)：%v", pos, err)
			}
		}
		if err := rc.SetBit(ctx, b.key, int64(positions[0]), 0).Err(); err != nil {
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
// REDIS_CLUSTER_ADDRS 未设置时跳过（内部测试不能 import test 包——依赖图成环，
// 此处自建守卫）。
func TestBitmapClusterFallbackRouting(t *testing.T) {
	raw := os.Getenv("REDIS_CLUSTER_ADDRS")
	if raw == "" {
		t.Skip("REDIS_CLUSTER_ADDRS not set; skip cluster test")
	}
	var addrs []string
	for _, p := range strings.Split(raw, ",") {
		if a := strings.TrimSpace(p); a != "" {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		t.Skip("REDIS_CLUSTER_ADDRS empty; skip cluster test")
	}

	opts := []Option{WithAddrs(addrs)}
	if pwd := os.Getenv("REDIS_PASSWORD"); pwd != "" {
		opts = append(opts, WithPassword(pwd))
	}
	rdb := New(opts...)
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
