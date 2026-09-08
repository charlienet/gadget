package redis

import (
	"errors"
	"testing"
)

// 本文件覆盖能力探测（capability.go）的纯逻辑层：模块行解析、模块名匹配、
// 命令族缓存字段的判定读取、COMMAND INFO 回复解析。
//
// 构造 Capability 时一律带 ready: true：ensureLoaded 见 ready 即直接返回，
// 不会触发 Probe（否则 rdb 为 nil 会炸、或发出真实网络命令）。这也是
// HasCuckoo 等判定与 HasModule 共用同一"先 ensureLoaded 再加锁"结构的
// 前提。
//
// 端到端探测（miniredis）回归见 redis_test.go 的 TestCapabilityModules。

func TestParseModuleLine(t *testing.T) {
	cases := []struct {
		name        string
		line        string
		wantName    string
		wantVersion string
	}{
		{"标准 bf 行", "name=bf,ver=20200,api=1,filters=0,usedby=[],usedbyme=[],support=[]", "bf", "20200"},
		{"RedisBloom 整包名", "name=redisbloom,ver=20405,api=1", "redisbloom", "20405"},
		{"大小写与真实现名", "name=ReJSON,ver=20000,api=1,filters=0", "ReJSON", "20000"},
		{"无 ver 字段", "name=search", "search", ""},
		{"无 name 字段", "ver=1,api=1", "", "1"},
		{"片段缺 = 被跳过", "name=topk,junk,ver=7", "topk", "7"},
		{"空行", "", "", ""},
		{"仅逗号", ",,", "", ""},
		// 字段顺序无关：ver 在前也能取到 name。
		{"字段顺序无关", "ver=10000,name=bf,api=1", "bf", "10000"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseModuleLine(c.line)
			if got.Name != c.wantName || got.Version != c.wantVersion {
				t.Fatalf("parseModuleLine(%q) = (%q,%q)，want (%q,%q)",
					c.line, got.Name, got.Version, c.wantName, c.wantVersion)
			}
		})
	}
}

// TestHasModule 验证模块名匹配口径：EqualFold 全名匹配（子串不算命中）。
func TestHasModule(t *testing.T) {
	c := &Capability{
		ready: true,
		modules: []moduleInfo{
			{Name: "bf", Version: "20200"},
			{Name: "ReJSON", Version: "20000"},
			{Name: "timeseries"},
		},
	}

	if !c.HasModule("bf") {
		t.Fatal(`HasModule("bf") 应为 true`)
	}
	// 大小写不敏感（EqualFold）
	if !c.HasModule("BF") || !c.HasModule("rejson") || !c.HasModule("TimeSeries") {
		t.Fatal("HasModule 应大小写不敏感")
	}
	// 真实缺陷根因：命令前缀不是模块名，按前缀查必为 false
	for _, prefix := range []string{"cf", "cms", "topk", "tdigest"} {
		if c.HasModule(prefix) {
			t.Fatalf("HasModule(%q) 应为 false——命令前缀不是 INFO MODULES 里的模块名", prefix)
		}
	}
	// 子串不得误命中（全名匹配）
	if c.HasModule("jso") || c.HasModule("bloomberg") {
		t.Fatal("HasModule 必须全名匹配，不能子串命中")
	}
	if c.HasModule("") {
		t.Fatal(`HasModule("") 应为 false`)
	}
	// 未加载模块
	if c.HasModule("graph") {
		t.Fatal(`HasModule("graph") 应为 false（未加载）`)
	}
}

// TestBloomFamilyCacheFields 钉死四判定的读取口径：各读各的缓存字段，
// 与 modules 列表内容无关（v0.7.0 修复点：此前 HasCuckoo 等查模块名
// "cf" 恒 false）。
func TestBloomFamilyCacheFields(t *testing.T) {
	c := &Capability{
		ready:      true,
		modules:    []moduleInfo{{Name: "bf"}},
		hasCuckoo:  true,
		hasCMS:     false,
		hasTopK:    true,
		hasTDigest: false,
	}

	if !c.HasBloom() {
		t.Fatal("HasBloom 应为 true（bf 在 modules 中）")
	}
	if !c.HasCuckoo() {
		t.Fatal("HasCuckoo 应读 hasCuckoo 缓存 = true（modules 中无 cf 条目不影响）")
	}
	if c.HasCMS() {
		t.Fatal("HasCMS 应读 hasCMS 缓存 = false")
	}
	if !c.HasTopK() {
		t.Fatal("HasTopK 应读 hasTopK 缓存 = true")
	}
	if c.HasTDigest() {
		t.Fatal("HasTDigest 应读 hasTDigest 缓存 = false")
	}

	// 逐字段独立：翻转任意一个不得影响其他三个
	c.hasCuckoo = false
	c.hasCMS = true
	if c.HasCuckoo() || !c.HasCMS() || !c.HasTopK() || c.HasTDigest() {
		t.Fatalf("四判定发生交叉污染：cuckoo=%v cms=%v topk=%v tdigest=%v",
			c.HasCuckoo(), c.HasCMS(), c.HasTopK(), c.HasTDigest())
	}
}

// TestValkeyBloomScenario（必录）：valkey-bloom / Redis 8 内建 bloom 形态
// ——bf 模块条目在场，但 CF/CMS/TOPK/TDIGEST 命令族均不存在（探测确认
// 后四缓存留 false）。期望 HasBloom 为 true、其余四判定为 false。
//
// 该场景正是"bf 在场却不该假定四族齐全"的分层探测动机；同时反向验证
// 修复没有把 HasBloom 一起改坏（BF.* 仍以模块在场为口径）。
func TestValkeyBloomScenario(t *testing.T) {
	c := &Capability{
		ready:   true,
		modules: []moduleInfo{{Name: "bf", Version: "10000"}},
		// hasCuckoo/hasCMS/hasTopK/hasTDigest 保持零值 false：probeCommandFamily
		// 探测后写回的结果即全部不存在。
	}

	if !c.HasBloom() {
		t.Fatal("valkey-bloom 形态下 HasBloom 必须为 true")
	}
	for name, got := range map[string]bool{
		"HasCuckoo":  c.HasCuckoo(),
		"HasCMS":     c.HasCMS(),
		"HasTopK":    c.HasTopK(),
		"HasTDigest": c.HasTDigest(),
	} {
		if got {
			t.Fatalf("%s 应为 false（bf 在场不代表四命令族齐全）", name)
		}
	}
}

// TestParseCommandInfoResponse 表驱动覆盖 COMMAND INFO 回复解析的纯函数。
func TestParseCommandInfoResponse(t *testing.T) {
	sentinel := errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")

	cases := []struct {
		name    string
		val     any
		err     error
		want    bool
		wantErr bool // 是否要求返回 error
	}{
		{
			name: "命令存在",
			val:  []any{[]any{"cf.add", int64(-2), int64(0), int64(0), int64(0), nil, nil}},
			want: true,
		},
		{
			name: "RESP3 形态（元素类型不同）",
			val:  []any{[]any{"BF.ADD", int64(-3), "write"}},
			want: true,
		},
		{
			name: "命令不存在（单元素 nil 数组）",
			val:  []any{nil},
			want: false,
		},
		{
			name: "空数组",
			val:  []any{},
			want: false,
		},
		{
			name: "nil 值（Do 无回复）",
			val:  nil,
			want: false,
		},
		{
			name: "非数组类型（代理异形回复，防御为不存在）",
			val:  "OK",
			want: false,
		},
		{
			name: "整数回复（防御为不存在）",
			val:  int64(1),
			want: false,
		},
		{
			name:    "错误透传（不得当作 false 缓存）",
			err:     sentinel,
			want:    false,
			wantErr: true,
		},
		{
			name:    "错误优先于值",
			val:     []any{[]any{"cf.add"}},
			err:     sentinel,
			want:    false,
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseCommandInfoResponse(c.val, c.err)
			if got != c.want {
				t.Fatalf("parseCommandInfoResponse(%#v, %v) = %v，want %v", c.val, c.err, got, c.want)
			}
			if c.wantErr {
				if !errors.Is(err, c.err) {
					t.Fatalf("错误应原样透传（调用方据此区分探测失败与不支持），got %v", err)
				}
			} else if err != nil {
				t.Fatalf("不应返回 error，got %v", err)
			}
		})
	}
}

// TestRefreshKeepsDataAndClearsReady 验证缓存清零的职责划分：Refresh 只
// 丢弃就绪标记、不改数据，数据清零由下一轮 probeLocked 在探测前完成
// （见 probeLocked 的"先清缓存"段），避免重试路径残留上一轮的 true。
func TestRefreshKeepsDataAndClearsReady(t *testing.T) {
	c := &Capability{ready: true, hasCuckoo: true, hasTopK: true}
	c.Refresh()
	if !c.hasCuckoo || !c.hasTopK {
		t.Fatal("Refresh 不应直接改动缓存字段（清零由下次 probeLocked 负责）")
	}
	if c.ready {
		t.Fatal("Refresh 应使 ready=false，触发下次重新探测")
	}
}
