package redis

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/zeebo/xxh3"
)

// newSimCuckoo 构造 client 为 nil 的纯计算 hashImpl（仅调 cuckooHashs /
// hashFingerprint 等不触网方法，模式参照 bloom_internal_test.go 的
// newSimBitmap）。
func newSimCuckoo(capacity, bucketSize int64) *hashImpl {
	cfg := defaultCuckooConfig()
	cfg.capacity = capacity
	cfg.bucketSize = bucketSize
	return newHashImpl(nil, "sim:cf", cfg)
}

// TestCuckooHashsDeterministic 断言 fp/i1/i2 的确定性、值域与哈希源口径：
// v0.7.0 起主哈希为 xxh3.Hash（xxh3-64，旧实现为 fnv1a，属 BREAKING，见
// cuckoo.go 的 hashImpl 注释）。若哈希源被替换回其他实现，"i1 ==
// xxh3.Hash(marshalItem(item)) % numBuckets" 的特征断言立即变红；i2 的
// 派生（hashFingerprint 乘法哈希 + 模加）本次未改，一并钉死。
func TestCuckooHashsDeterministic(t *testing.T) {
	h := newSimCuckoo(1000, 4) // numBuckets = 1000/4 = 250
	if h.numBuckets != 250 {
		t.Fatalf("numBuckets got %d want 250", h.numBuckets)
	}

	for i := range 500 {
		item := fmt.Sprintf("det-%d", i)

		fp1, i11, i21, err := h.cuckooHashs(item)
		if err != nil {
			t.Fatalf("cuckooHashs(%s)：%v", item, err)
		}
		fp2, i12, i22, err := h.cuckooHashs(item)
		if err != nil {
			t.Fatalf("cuckooHashs(%s) 重复调用：%v", item, err)
		}
		if fp1 != fp2 || i11 != i12 || i21 != i22 {
			t.Fatalf("哈希不确定：%s 两次结果 (%d,%d,%d) vs (%d,%d,%d)", item, fp1, i11, i21, fp2, i12, i22)
		}

		// 值域：fp ∈ [1, 0xFFFF]（0 被归一为 1，空槽哨兵）；i1/i2 ∈ [0, n)
		if fp1 < 1 || fp1 > 0xFFFF {
			t.Fatalf("fp 越界：%s fp=%d", item, fp1)
		}
		if i11 < 0 || i11 >= h.numBuckets {
			t.Fatalf("i1 越界：%s i1=%d n=%d", item, i11, h.numBuckets)
		}
		if i21 < 0 || i21 >= h.numBuckets {
			t.Fatalf("i2 越界：%s i2=%d n=%d", item, i21, h.numBuckets)
		}

		// 哈希源特征：与 xxh3.Hash(marshalItem(item)) 直接推导一致
		data, err := marshalItem(item)
		if err != nil {
			t.Fatalf("marshalItem(%s)：%v", item, err)
		}
		sum := xxh3.Hash(data)
		if want := int64(sum % uint64(h.numBuckets)); i11 != want {
			t.Fatalf("i1 与 xxh3-64 口径分叉：%s got %d want %d（哈希源被改？）", item, i11, want)
		}
		wantFp := int64(sum & 0xFFFF)
		if wantFp == 0 {
			wantFp = 1
		}
		if fp1 != wantFp {
			t.Fatalf("fp 与 xxh3-64 口径分叉：%s got %d want %d", item, fp1, wantFp)
		}
		// i2 派生关系不变（模加候选桶）
		if want := (i11 + h.hashFingerprint(fp1)) % h.numBuckets; i21 != want {
			t.Fatalf("i2 派生关系破坏：%s got %d want %d", item, i21, want)
		}
	}
}

// TestCuckooHashsUnsupportedItem 断言不支持类型返回数据类错误
// （不 panic），fp/i1/i2 归零。
func TestCuckooHashsUnsupportedItem(t *testing.T) {
	h := newSimCuckoo(1000, 4)
	fp, i1, i2, err := h.cuckooHashs(struct{ X int }{1})
	if err == nil {
		t.Fatalf("struct 入参应报错，got fp=%d i1=%d i2=%d", fp, i1, i2)
	}
	if IsUnavailable(err) {
		// 编码错误必须是数据类错误，不得触发 FailPolicy 兜底
		t.Fatalf("编码错误不得判为服务不可用：%v", err)
	}
	if fp != 0 || i1 != 0 || i2 != 0 {
		t.Fatalf("报错时返回值应归零，got (%d,%d,%d)", fp, i1, i2)
	}
}

// TestCuckooHashsDistribution 验证 xxh3-64 的散布质量——本次换哈希的核心
// 动机：FNV-1a 低位雪崩质量差，i1 = h % numBuckets 在 numBuckets 为 2 的幂
// 时只用低位，桶分布偏斜、fp（低 16 位）相关性聚集。这里刻意取
// numBuckets = 1024/4 = 256（2 的幂）作为最容易暴露偏斜的场景：
//   - i1 无空桶、每桶计数在期望 ±50% 内（Poisson 尾概率 < 1e-10，非 flaky）；
//   - fp 去重数远超 FNV-1a 低 16 位在顺序字符串输入下的聚集水平。
func TestCuckooHashsDistribution(t *testing.T) {
	h := newSimCuckoo(1024, 4) // numBuckets = 256（2 的幂，最低位的雪崩检验场）
	if h.numBuckets != 256 {
		t.Fatalf("numBuckets got %d want 256", h.numBuckets)
	}

	const total = 50_000
	buckets := make([]int, h.numBuckets)
	fpSeen := make(map[int64]struct{}, 1<<16)
	for i := range total {
		fp, i1, _, err := h.cuckooHashs(fmt.Sprintf("dist-%d", i))
		if err != nil {
			t.Fatalf("cuckooHashs dist-%d：%v", i, err)
		}
		buckets[i1]++
		fpSeen[fp] = struct{}{}
	}

	exp := total / int(h.numBuckets) // 195
	empty, skew := 0, 0
	for idx, c := range buckets {
		if c == 0 {
			empty++
		}
		if c < exp/2 || c > exp+exp/2 {
			skew++
			t.Logf("桶 %d 计数 %d 超出期望 %d ±50%%", idx, c, exp)
		}
	}
	if empty > 0 {
		t.Fatalf("%d 个桶完全未被命中（低位雪崩不足，桶索引偏斜）", empty)
	}
	if skew > 0 {
		t.Fatalf("%d 个桶计数超出 ±50%% 容差（xxh3-64 散布质量回归）", skew)
	}

	// fp 去重：50000 次抽样、65535 值域的期望去重数 ≈ 36000；FNV-1a 低 16
	// 位在顺序输入下明显聚集，取 20000 为宽松下限（远离随机噪声、足以
	// 抓住回归）。
	if len(fpSeen) < 20000 {
		t.Fatalf("fp 去重数过低：%d（<20000，低 16 位相关性聚集——哈希源疑被换回弱雪崩实现）", len(fpSeen))
	}
	t.Logf("i1 散布：%d 桶 / 期望 %d 每桶，fp 去重 %d/%d", h.numBuckets, exp, len(fpSeen), total)
}

// TestCfCmdImplGateRearmOnReset 钉死模块版 Reset 的 CF.RESERVE 闸门生命周期
// （miniredis 离线探针，不依赖真实 RedisBloom）。
//
// 探针原理：miniredis 不支持 CF.*，ensureReserve 发出的 CF.RESERVE 必以
// 命令级错误失败（非 Unavailable、不被 "item exists" 吞掉），恰好可作为
// "闭包是否执行"的信号——失败后闸门解除武装（任务 6 起），Reset 换入新
// sync.Once 后闭包同样重新执行 → 再次报命令错误。
// 以此证明"Reset 后闸门复位、CF.RESERVE 会重发"（重发所用参数正确性由
// cuckoo_test.go 的集成探针在有真实 RedisBloom 时验证）。
//
// 分派控制：手动置 Capability 缓存（ready+hasCuckoo）令 NewCuckooFilter
// 走 cfCmdImpl 分支；DEL 是通用命令，miniredis 可执行。
func TestCfCmdImplGateRearmOnReset(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	base := New(WithAddr(mr.Addr()))
	rc, ok := base.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", base)
	}
	rc.cap = &Capability{ready: true, hasCuckoo: true}
	defer func() { _ = rc.GracefulClose(context.Background()) }()

	ctx := context.Background()
	cf := rc.NewCuckooFilter("cfi:gate", WithCuckooCapacity(100), WithBucketSize(3))
	impl, ok := cf.impl.(*cfCmdImpl)
	if !ok {
		t.Fatalf("分派异常：%T（期望 *cfCmdImpl）", cf.impl)
	}

	// ① 构造不变量：闸门非 nil——atomic.Pointer 零值 Load 返回 nil，
	// 工厂构造点漏 Store(new(sync.Once)) 会在 ensureReserve 处 panic。
	o1 := impl.once.Load()
	if o1 == nil {
		t.Fatal("工厂构造的 cfCmdImpl 闸门为 nil——构造点缺 Store(new(sync.Once))")
	}

	// ② 首次 ensureReserve：闭包执行、发出 CF.RESERVE → miniredis 报命令级
	// 错误（非 Unavailable、不被吞）。
	err1 := impl.ensureReserve(ctx)
	if err1 == nil || IsUnavailable(err1) {
		t.Fatalf("首次 ensureReserve 应发出 CF.RESERVE 并因 unknown command 失败，got %v", err1)
	}
	// 闸门失败即解除武装（任务 6，专项回归见 TestCfCmdImplGateDisarmOnFailure）：
	// 第二次调用重新执行闭包 → 再次命令错误。
	if err := impl.ensureReserve(ctx); err == nil || IsUnavailable(err) {
		t.Fatalf("闸门失败后应解除武装并重发 CF.RESERVE，got %v", err)
	}

	// ③ Reset：DEL 成功（通用命令），并换入新闸门。
	if err := impl.Reset(ctx); err != nil {
		t.Fatalf("Reset（DEL 成功路径）：%v", err)
	}
	o2 := impl.once.Load()
	if o2 == nil || o2 == o1 {
		t.Fatal("Reset 未换入新闸门（once 指针身份未变或为 nil）")
	}

	// ④ 复位后 ensureReserve 重新执行闭包 → 重发 CF.RESERVE（再次命令错误）。
	// 注：任务 6 起本探针与解除武装路径信号同形（失败也会重发），此处仍
	// 保留断言以钉死 Reset 后闸门为新对象的事实（身份断言在 ③）。
	err2 := impl.ensureReserve(ctx)
	if err2 == nil || IsUnavailable(err2) {
		t.Fatalf("闸门复位后应重发 CF.RESERVE，got %v", err2)
	}
}

// TestCfCmdImplGateRearmOnFailedReset 钉死"无条件复位"决策：DEL 因服务宕机
// 失败时 Reset 仍返回错误，但闸门必须照常换新。"仅成功才复位"的隐患：DEL
// 实际已执行而客户端收到网络错误 → 旧闸门燃尽残留 → 实例后续 Add 不再
// RESERVE，过滤器被模块默认参数（capacity=100、bucketSize=2、
// maxIterations=20）隐式重建，With* 配置静默作废；双 RESERVE 竞态的代价
// 只是被 ensureReserve 吞掉的 "item exists"，无害。
func TestCfCmdImplGateRearmOnFailedReset(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	addr := mr.Addr()

	base := New(WithAddr(addr))
	rc, ok := base.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", base)
	}
	defer func() { _ = rc.GracefulClose(context.Background()) }()

	impl := &cfCmdImpl{client: rc, key: "cfi:gate-fail", cfg: defaultCuckooConfig()}
	impl.once.Store(new(sync.Once))

	ctx := context.Background()
	old := impl.once.Load()
	old.Do(func() {}) // 耗尽当代闸门（模拟 RESERVE 已执行过的状态）

	mr.Close() // 服务宕机：DEL 产生网络层错误
	err = impl.Reset(ctx)
	if err == nil {
		t.Fatal("宕机下 Reset 的 DEL 应返回错误")
	}
	if !IsUnavailable(err) {
		t.Fatalf("宕机错误应判为 Unavailable（门面据此包装哨兵），got %v", err)
	}

	// 失败仍无条件复位：换入的新闸门可再次触发闭包。
	next := impl.once.Load()
	if next == old {
		t.Fatal("DEL 失败后闸门仍应无条件复位（旧燃尽闸门残留将致默认参数隐式重建）")
	}
	fired := false
	next.Do(func() { fired = true })
	if !fired {
		t.Fatal("复位后的闸门不可再触发（new(sync.Once) 未正确换入？）")
	}
}

// TestCfCmdImplGateDisarmOnFailure 钉死闸门失败解除武装（任务 6）：
// CF.RESERVE 报非 "item exists"/"already exists" 类错误（miniredis 下为
// unknown command）时，ensureReserve 必须换掉本代闸门，下一次调用重试
// CF.RESERVE——sync.Once 不辨成败，不解除武装则一次瞬态失败永久燃尽闸门，
// 恢复后过滤器被模块默认参数隐式建立，With* 配置静默作废。
// 每次失败都应换入新对象（连续失败不积累旧状态）。
// "exists 类吞错维持武装"分支由 cuckoo_test.go 集成用例覆盖（需真实
// RedisBloom 产生 "Item already exists"；miniredis v2.5.0 无法注入自定义
// 错误文本）。
func TestCfCmdImplGateDisarmOnFailure(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	base := New(WithAddr(mr.Addr()))
	rc, ok := base.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", base)
	}
	rc.cap = &Capability{ready: true, hasCuckoo: true}
	defer func() { _ = rc.GracefulClose(context.Background()) }()

	ctx := context.Background()
	cf := rc.NewCuckooFilter("cfi:disarm", WithCuckooCapacity(100))
	impl, ok := cf.impl.(*cfCmdImpl)
	if !ok {
		t.Fatalf("分派异常：%T（期望 *cfCmdImpl）", cf.impl)
	}

	// 首次 ensureReserve：CF.RESERVE → unknown command 失败 → 解除武装。
	g0 := impl.once.Load()
	err = impl.ensureReserve(ctx)
	if err == nil || IsUnavailable(err) {
		t.Fatalf("首次 ensureReserve 应报命令级错误，got %v", err)
	}
	g1 := impl.once.Load()
	if g1 == nil || g1 == g0 {
		t.Fatal("RESERVE 失败后闸门未解除武装（旧燃尽 Once 残留，恢复后将永不再试）")
	}

	// 第二次：闭包重执行（重试 CF.RESERVE）→ 再次失败 → 再次解除武装。
	err = impl.ensureReserve(ctx)
	if err == nil || IsUnavailable(err) {
		t.Fatalf("解除武装后第二次 ensureReserve 应重发 CF.RESERVE 并再次命令错误，got %v", err)
	}
	g2 := impl.once.Load()
	if g2 == nil || g2 == g1 {
		t.Fatal("第二次失败后闸门应再次换入新对象")
	}

	// 可触发性：当代闸门 Do 可执行（未燃尽）。
	fired := false
	g2.Do(func() { fired = true })
	if !fired {
		t.Fatal("当代闸门不可触发（解除武装换入的不是全新 sync.Once？）")
	}
}

// ---------------------------------------------------------------------------
// 真环境 hashImpl（降级实现）全方法覆盖
//
// 背景缺口：带 RedisBloom 的真实服务器上工厂 auto 分派恒选 cfCmdImpl，
// hashImpl 经公开路径物理不可达（无 WithCuckooImpl 强制项，裁决不开）。
// 本区白盒直构 newHashImpl 连真实实例补齐覆盖——先例：bloom_internal_test.go
// 的 newBitmapImpl 真机用例（TestBitmapConcurrentAddRealRedis /
// TestBitmapClusterFallbackRouting）。
//
// 共享实例纪律：key 带 "cuckootest:" 前缀 + UnixNano 唯一段，收尾 Del；
// 严禁 FLUSHDB/FLUSHALL。内部测试不能 import test 包（依赖图成环），
// 守卫自建（os.Getenv + NewWithUrl）。
// ---------------------------------------------------------------------------

// cuckooTestKey 生成共享实例上的隔离测试 key（前缀 + 纳秒唯一段 + 语义后缀）。
func cuckooTestKey(suffix string) string {
	return fmt.Sprintf("cuckootest:%d:%s", time.Now().UnixNano(), suffix)
}

// newRealStandaloneClient 连 REDIS_URL 真实单机实例；未设置时 skip。
func newRealStandaloneClient(t *testing.T) *redisClient {
	t.Helper()
	raw := os.Getenv("REDIS_URL")
	if raw == "" {
		t.Skip("REDIS_URL not set; skip real-Redis test")
	}
	rdb, err := NewWithUrl(raw)
	if err != nil {
		t.Fatalf("REDIS_URL 解析失败：%v", err)
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}
	t.Cleanup(func() { _ = rc.GracefulClose(context.Background()) })
	return rc
}

// newRealClusterClient 连 REDIS_CLUSTER 真实集群；未设置时 skip。
func newRealClusterClient(t *testing.T) *redisClient {
	t.Helper()
	raw := os.Getenv("REDIS_CLUSTER")
	if raw == "" {
		t.Skip("REDIS_CLUSTER not set; skip cluster test")
	}
	rdb, err := NewWithUrl(raw)
	if err != nil {
		t.Fatalf("REDIS_CLUSTER URL 解析失败：%v", err)
	}
	rc, ok := rdb.(*redisClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rdb)
	}
	t.Cleanup(func() { _ = rc.GracefulClose(context.Background()) })
	return rc
}

// runHashImplSuite 在给定真实 client 环境上跑 hashImpl 全 9 方法用例组
// （基本流 / Reset / 规模+并发）。单机与集群共用同一断言集——hashImpl 为
// 单键形态（HSET/EVAL 均单 KEYS[1]），集群上无 CROSSSLOT；两环境行为应
// 逐字节一致，环境分叉即缺陷。集群特有的路由/脚本缓存语义见 extra 子测试。
func runHashImplSuite(t *testing.T, envName string, rc *redisClient) {
	t.Helper()
	ctx := context.Background()

	// ---- 组 1：基本流（9 方法全触达） ----
	t.Run(envName+"/基本流 全 9 方法", func(t *testing.T) {
		key := cuckooTestKey(envName + "-basic")
		t.Cleanup(func() { _ = rc.Del(ctx, key).Err() })

		cfg := defaultCuckooConfig()
		cfg.capacity = 100000
		h := newHashImpl(rc, key, cfg)

		// Add → Exists → Count → Del → Exists false → Count 0
		added, err := h.Add(ctx, "s-1")
		if err != nil || !added {
			t.Fatalf("Add(s-1)：added=%v err=%v", added, err)
		}
		if ok, err := h.Exists(ctx, "s-1"); err != nil || !ok {
			t.Fatalf("Exists(s-1)：ok=%v err=%v", ok, err)
		}
		if n, err := h.Count(ctx, "s-1"); err != nil || n != 1 {
			t.Fatalf("Count(s-1)：n=%d err=%v（回退版去重语义应为 1）", n, err)
		}
		if del, err := h.Del(ctx, "s-1"); err != nil || !del {
			t.Fatalf("Del(s-1)：del=%v err=%v", del, err)
		}
		if ok, err := h.Exists(ctx, "s-1"); err != nil || ok {
			t.Fatalf("Del 后 Exists(s-1) 应为 false：ok=%v err=%v", ok, err)
		}
		if n, err := h.Count(ctx, "s-1"); err != nil || n != 0 {
			t.Fatalf("Del 后 Count(s-1) 应为 0：n=%d err=%v", n, err)
		}

		// Info NumItems 变化
		info0, err := h.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range []string{"i-1", "i-2", "i-3"} {
			if _, err := h.Add(ctx, it); err != nil {
				t.Fatal(err)
			}
		}
		info1, err := h.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info1.NumItems != info0.NumItems+3 {
			t.Fatalf("Info NumItems：%d → %d（期望 +3）", info0.NumItems, info1.NumItems)
		}

		// AddNX：首次 true、重复 false（回退版与 Add 等价）
		if added, err := h.AddNX(ctx, "nx-1"); err != nil || !added {
			t.Fatalf("AddNX 首次：added=%v err=%v", added, err)
		}
		if added, err := h.AddNX(ctx, "nx-1"); err != nil || added {
			t.Fatalf("AddNX 重复应 false：added=%v err=%v", added, err)
		}

		// ExistsMulti：顺序锚定（与逐条 Exists 全等）
		queries := []any{"i-1", "ghost-1", "i-2", "nx-1", "ghost-2"}
		got, err := h.ExistsMulti(ctx, queries...)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(queries) {
			t.Fatalf("ExistsMulti 长度 %d，期望 %d", len(got), len(queries))
		}
		for i, q := range queries {
			want, err := h.Exists(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if want != got[i] {
				t.Fatalf("ExistsMulti 第 %d 项（%v）与逐条 Exists 分叉：%v vs %v", i, q, want, got[i])
			}
		}

		// AddMulti：批量语义（新增 true / 已存在 false / 顺序对应）
		res, err := h.AddMulti(ctx, "m-1", "i-1", "m-2")
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 3 || !res[0] || res[1] || !res[2] {
			t.Fatalf("AddMulti 结果 %v，期望 [true false true]", res)
		}
	})

	// ---- 组 2：Reset ----
	t.Run(envName+"/Reset 清空与幂等", func(t *testing.T) {
		key := cuckooTestKey(envName + "-reset")
		t.Cleanup(func() { _ = rc.Del(ctx, key).Err() })

		cfg := defaultCuckooConfig()
		cfg.capacity = 10000
		h := newHashImpl(rc, key, cfg)

		var items []any
		for i := range 50 {
			items = append(items, fmt.Sprintf("rs-%d", i))
		}
		if _, err := h.AddMulti(ctx, items...); err != nil {
			t.Fatal(err)
		}
		if err := h.Reset(ctx); err != nil {
			t.Fatalf("Reset：%v", err)
		}
		// 清空后全部查不到
		hit, err := h.ExistsMulti(ctx, items...)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range hit {
			if v {
				t.Fatalf("Reset 后第 %d 项仍命中", i)
			}
		}
		// Info 零值
		info, err := h.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.NumItems != 0 || info.NumBuckets != 0 {
			t.Fatalf("Reset 后 Info 应零值：items=%d buckets=%d", info.NumItems, info.NumBuckets)
		}
		// 物理键不存在
		if n, err := rc.Exists(ctx, key).Result(); err != nil || n != 0 {
			t.Fatalf("Reset 后 EXISTS 应为 0：n=%d err=%v", n, err)
		}
		// 重写可用
		if added, err := h.Add(ctx, "after"); err != nil || !added {
			t.Fatalf("Reset 后 Add：added=%v err=%v", added, err)
		}
		// 幂等：连两次 Reset 无错
		if err := h.Reset(ctx); err != nil {
			t.Fatalf("二次 Reset：%v", err)
		}
		if err := h.Reset(ctx); err != nil {
			t.Fatalf("三次 Reset：%v", err)
		}
	})

	// ---- 组 4：规模 + 并发 ----
	t.Run(envName+"/10k 规模与并发冒烟", func(t *testing.T) {
		key := cuckooTestKey(envName + "-scale")
		t.Cleanup(func() { _ = rc.Del(ctx, key).Err() })

		cfg := defaultCuckooConfig()
		cfg.capacity = 1000000
		h := newHashImpl(rc, key, cfg)

		// 10k 插入（10 批 ×1000 AddMulti），记录耗时
		start := time.Now()
		for batch := range 10 {
			items := make([]any, 1000)
			for j := range items {
				items[j] = fmt.Sprintf("sc-%d-%d", batch, j)
			}
			res, err := h.AddMulti(ctx, items...)
			if err != nil {
				t.Fatalf("批次 %d：%v", batch, err)
			}
			for j, v := range res {
				if !v {
					t.Fatalf("批次 %d 第 %d 项低负载下插入失败", batch, j)
				}
			}
		}
		t.Logf("%s：10k 插入（10×AddMulti1000）耗时 %v", envName, time.Since(start))

		// 全量抽查命中（无假阴性）
		sample := make([]any, 1000)
		for j := range sample {
			sample[j] = fmt.Sprintf("sc-3-%d", j)
		}
		hit, err := h.ExistsMulti(ctx, sample...)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range hit {
			if !v {
				t.Fatalf("已插入样本第 %d 项假阴性（cuckoo 不允许）", i)
			}
		}

		// 阶段 1：20 goroutine Add×Exists（无 Reset 干扰，世代稳定）——
		// 强断言：Add 成功后立即 Exists 必命中（无假阴性）
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for g := range 20 {
			wg.Go(func() {
				<-gate
				for i := range 25 {
					item := fmt.Sprintf("conc-%d-%d", g, i)
					if _, err := h.Add(ctx, item); err != nil {
						t.Errorf("并发 Add %s：%v", item, err)
						return
					}
					if ok, err := h.Exists(ctx, item); err != nil {
						t.Errorf("并发 Exists %s：%v", item, err)
						return
					} else if !ok {
						t.Errorf("并发 Add 成功后 Exists %s 假阴性", item)
						return
					}
				}
			})
		}
		close(gate)
		wg.Wait()

		// 阶段 2：Reset 并发冒烟（Add×Reset 混跑）。Reset 与并发写入无
		// 相对次序承诺——元素可能落在清空前后世代，故本阶段不断言结果，
		// 只验证无错误/无 panic/无死锁。
		for g := range 20 {
			wg.Go(func() {
				_, _ = h.Add(ctx, fmt.Sprintf("smoke-%d", g))
			})
		}
		for range 3 {
			wg.Go(func() {
				if err := h.Reset(ctx); err != nil {
					t.Errorf("并发 Reset：%v", err)
				}
			})
		}
		wg.Wait()
		// 收尾：最终一次 Reset 后键必须不存在
		if err := h.Reset(ctx); err != nil {
			t.Fatalf("收尾 Reset：%v", err)
		}
		if n, err := rc.Exists(ctx, key).Result(); err != nil || n != 0 {
			t.Fatalf("收尾 Reset 后 EXISTS=%d err=%v", n, err)
		}
	})
}

// TestHashImplRealStandalone 真单机 Redis 上 hashImpl 全方法覆盖
// （缺口裁决：工厂 auto 在带模块服务器上物理不可达 hashImpl，白盒直构）。
func TestHashImplRealStandalone(t *testing.T) {
	rc := newRealStandaloneClient(t)
	runHashImplSuite(t, "standalone", rc)

	// ---- 组 5：FPR sanity（仅真单机）----
	t.Run("standalone/FPR sanity", func(t *testing.T) {
		ctx := context.Background()
		key := cuckooTestKey("fpr")
		t.Cleanup(func() { _ = rc.Del(ctx, key).Err() })

		cfg := defaultCuckooConfig()
		cfg.capacity = 1_000_000 // 1M 容量预算（默认 bucketSize=4）
		h := newHashImpl(rc, key, cfg)

		// 插入 10k 项（1% 低负载，远离驱逐丢失区）
		for batch := range 10 {
			items := make([]any, 1000)
			for j := range items {
				items[j] = fmt.Sprintf("ins-%d-%d", batch, j)
			}
			if _, err := h.AddMulti(ctx, items...); err != nil {
				t.Fatal(err)
			}
		}

		// 探测 200k 从未插入项（前缀 "nop-" 与 "ins-" 天然不相交），
		// 200 批 ×1000 ExistsMulti。理论假阳上界 fpp = 2×bucketSize/2^16
		// （经典 cuckoo 公式：两候选桶 × 每桶 4 槽 × 每槽指纹碰撞 1/65535），
		// 1% 负载下实际远低于该上界；断言观测假阳 ≤ 2×上界期望：
		// 200000 × 8/65536 × 2 ≈ 48。
		const probes = 200_000
		fp := 0
		start := time.Now()
		for batch := range probes / 1000 {
			q := make([]any, 1000)
			for j := range q {
				q[j] = fmt.Sprintf("nop-%d-%d", batch, j)
			}
			res, err := h.ExistsMulti(ctx, q...)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range res {
				if v {
					fp++
				}
			}
		}
		observed := float64(fp) / probes
		ceiling := float64(2*h.bucketSize) / 65536 // 经典上界：两候选桶 × 每桶 bucketSize 槽
		t.Logf("FPR 实测：fp=%d/%d = %.7f（理论上界 %.7f，阈值 2×=%.7f），耗时 %v",
			fp, probes, observed, ceiling, 2*ceiling, time.Since(start))
		if limit := int(2 * ceiling * probes); fp > limit {
			t.Fatalf("假阳性超标：got %d > 2×理论上界 %d（fpp=%.3e）", fp, limit, ceiling)
		}
	})
}

// TestHashImplRealCluster 真集群 Redis 上 hashImpl 全方法覆盖。
// 集群特有锚定（组 3）：
//   - hashImpl 单键形态——全部命令（HGET/HSET/EVAL）围绕单 KEYS[1] 落同一
//     slot，无 CROSSSLOT；全链路命令成功即路由合法性证明。
//   - Lua EVAL 的集群脚本缓存行为：go-redis Script.Run 首次 EVALSHA 对新
//     连接/未缓存节点报 NOSCRIPT 后自动回退 EVAL——本用例从全新连接冷
//     启动，首轮 Add/Exists 即覆盖 NOSCRIPT→EVAL 迁移路径，跑通即锚定。
func TestHashImplRealCluster(t *testing.T) {
	rc := newRealClusterClient(t)
	runHashImplSuite(t, "cluster", rc)
}
