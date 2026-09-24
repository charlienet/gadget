package redis

import (
	"errors"
	"strings"
	"testing"
)

// v0.11.0 G4：NewCuckooFilter 的模块期望声明（WithCuckooModule）与
// CuckooInfo.Path 路径自检观测。用例与 bloom 侧镜像（错误哨兵共用
// bloom.go 的包级两个）。

// TestCuckooModuleCFNotProbed CF + 未探测 → ErrCapabilityNotProbed 且
// 零命令副作用（校验在 connectAll 之前）。
func TestCuckooModuleCFNotProbed(t *testing.T) {
	ctx := t.Context()
	rc, mr := newMiniRedisClient(t)

	baseline := mr.CommandCount()
	_, err := rc.NewCuckooFilter(ctx, "cm:notprobed", WithCuckooModule(CuckooModuleCF))
	if !errors.Is(err, ErrCapabilityNotProbed) {
		t.Fatalf("CF+未探测应报 ErrCapabilityNotProbed，got %v", err)
	}
	if got := mr.CommandCount(); got != baseline {
		t.Fatalf("校验失败须零命令副作用：CommandCount %d → %d", baseline, got)
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("校验失败不得建键，got %v", keys)
	}
}

// TestCuckooModuleCFProbedNoModule CF + 已探测但无 cf 命令族 →
// ErrModuleNotLoaded，错误串携带实际判定（HasCuckoo）。
func TestCuckooModuleCFProbedNoModule(t *testing.T) {
	ctx := t.Context()
	rc, mr := newMiniRedisClient(t)

	// bf 在场但 CF 命令族探测为 false（valkey-bloom 形态）。
	setProbedForTest(t, rc.cap, true, []moduleInfo{{Name: "bf"}}, false)

	baseline := mr.CommandCount()
	_, err := rc.NewCuckooFilter(ctx, "cm:nocf", WithCuckooModule(CuckooModuleCF))
	if !errors.Is(err, ErrModuleNotLoaded) {
		t.Fatalf("CF+探测无模块应报 ErrModuleNotLoaded，got %v", err)
	}
	if !strings.Contains(err.Error(), "HasCuckoo") {
		t.Fatalf("错误串应携带实际探测判定（HasCuckoo），got %v", err)
	}
	if got := mr.CommandCount(); got != baseline {
		t.Fatalf("报错同样须零命令副作用：CommandCount %d → %d", baseline, got)
	}
}

// TestCuckooModuleHashForcesHash Hash + 已探测有 cf → 无条件强制 hash
// 路径、成功构造（不查探测状态、不报错）；Hash+未探测同样成功。
// miniredis 不支持 CF.*，cf 路径构造必失败——成功即强制通道判据。
func TestCuckooModuleHashForcesHash(t *testing.T) {
	ctx := t.Context()
	rc, _ := newMiniRedisClient(t)

	setProbedForTest(t, rc.cap, true, []moduleInfo{{Name: "bf"}}, true) // hasCuckoo=true

	cf, err := rc.NewCuckooFilter(ctx, "cm:forcehash", WithCuckooModule(CuckooModuleHash))
	if err != nil {
		t.Fatalf("Hash 强制通道不得因探测态报错：%v", err)
	}
	if _, ok := cf.impl.(*hashImpl); !ok {
		t.Fatalf("应交付 hashImpl，got %T", cf.impl)
	}
	info, err := cf.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path != PathHash {
		t.Fatalf("hash 路径 Info().Path 应为 %q，got %q", PathHash, info.Path)
	}

	// 未探测同样无条件成功。
	rc2, _ := newMiniRedisClient(t)
	if _, err := rc2.NewCuckooFilter(ctx, "cm:forcehash2", WithCuckooModule(CuckooModuleHash)); err != nil {
		t.Fatalf("Hash+未探测应无条件成功：%v", err)
	}
}

// TestCuckooModuleAutoRegression Auto（零值）与现状一致：保守态走 hash
// 不报错（bf/hasCuckoo 在场的 Auto→cf 回归由既有工厂测试与真机用例
// TestCuckooModuleCFPathRealRedis 覆盖）。
func TestCuckooModuleAutoRegression(t *testing.T) {
	ctx := t.Context()
	rc, _ := newMiniRedisClient(t)

	cf, err := rc.NewCuckooFilter(ctx, "cm:auto", WithCuckooModule(CuckooModuleAuto))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cf.impl.(*hashImpl); !ok {
		t.Fatalf("Auto+未探测保守态应走 hash，got %T", cf.impl)
	}
}

// TestCuckooModuleCFPathRealRedis 真 Redis + cf 命令族（gate 同 bloom
// 侧）：CF 显式声明构造成功走 CF.* 路径，Info().Path==PathCF。
func TestCuckooModuleCFPathRealRedis(t *testing.T) {
	ctx := t.Context()
	rc := newRealStandaloneClient(t)

	if err := rc.Capability().Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !rc.Capability().HasCuckoo() {
		t.Skip("server has no cf command family; skip CF path real-Redis assertions")
	}

	key := cuckooTestKey("cf-path")
	t.Cleanup(func() { _ = rc.Del(ctx, key).Err() })

	cf, err := rc.NewCuckooFilter(ctx, key, WithCuckooModule(CuckooModuleCF))
	if err != nil {
		t.Fatalf("CF+已探测有模块应成功构造：%v", err)
	}
	if _, ok := cf.impl.(*cfCmdImpl); !ok {
		t.Fatalf("CF 声明应交付 cfCmdImpl，got %T", cf.impl)
	}
	info, err := cf.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path != PathCF {
		t.Fatalf("CF 路径 Info().Path 应为 %q，got %q", PathCF, info.Path)
	}
}
