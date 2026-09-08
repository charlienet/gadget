package redis

import (
	"fmt"
	"testing"

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
