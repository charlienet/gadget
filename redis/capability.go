package redis

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/hashicorp/go-version"
)

// Capability provides cached information about the Redis server's version
// and loaded modules. Probe(ctx) 是触发网络探测的唯一入口；全部查询方法
// （Version/VersionAtLeast/HasModule/HasXXX）均为纯内存读——未探测（或
// Refresh 之后）返回保守值（false/空串），不会自动发起命令。模块中途
// 装卸的生效路径为 Refresh()+Probe(ctx)。
type Capability struct {
	mu  sync.Mutex
	rdb *redisClient

	version    string
	versionSem *version.Version
	modules    []moduleInfo

	// probed 报告 probeLocked 是否**全部探测步骤成功**过（v0.11.0 G4）：
	// 区分「未探测」与「探测后无模块」两态，供工厂的模块期望校验
	// （WithModule/WithCuckooModule）使用。部分失败（任一步骤报错提前
	// 返回）保持 false——对齐「不得把探测失败当不支持缓存」口径；
	// Refresh 清除置位（回到未探测保守态）。
	probed bool

	// 命令族级能力缓存：由 probeLocked 在 bf 模块在场时经
	// `COMMAND INFO <族>.<命令>` 逐族真实确认，其余情况恒 false。**不得**
	// 再用模块名判定——INFO MODULES 里的模块名是 bf/cb/RedisBloom 等加载名，
	// 而 cf/cms/topk/tdigest 只是命令前缀，按前缀查模块名恒 false。
	hasCuckoo  bool
	hasCMS     bool
	hasTopK    bool
	hasTDigest bool
}

type moduleInfo struct {
	Name    string
	Version string
}

func newCapability(rdb *redisClient) *Capability {
	return &Capability{rdb: rdb}
}

// Probe sends INFO commands to the server and caches version and module info.
// 探测失败（如服务器不可达）时返回真实错误，调用方决定重试时机；
// 查询方法不感知探测状态，始终返回当前缓存值。
func (c *Capability) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.probeLocked(ctx)
}

// Refresh 清除已缓存的能力数据（查询回到保守态 false/空串）；重探须显式
// Probe(ctx)——模块中途装卸的生效路径为 Refresh()+Probe(ctx)。
func (c *Capability) Refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.version, c.versionSem, c.modules = "", nil, nil
	c.hasCuckoo, c.hasCMS, c.hasTopK, c.hasTDigest = false, false, false, false
	c.probed = false
}

func (c *Capability) probeLocked(ctx context.Context) error {
	// 重探即失效置位：任何一步失败都停留在 false（成功路径末尾统一置
	// true），不把半途失败当作可信探测结果。
	c.probed = false

	// --- Server version ---
	info, err := c.rdb.Info(ctx, "Server").Result()
	if err != nil {
		return err
	}
	c.version = ""
	c.versionSem = nil
	for line := range strings.SplitSeq(info, "\r\n") {
		if after, found := strings.CutPrefix(line, "redis_version:"); found {
			c.version = after
			if v, err := version.NewVersion(after); err == nil {
				c.versionSem = v
			}
			break
		}
	}

	// --- Modules ---
	modInfo, err := c.rdb.Info(ctx, "Modules").Result()
	if err != nil {
		return err
	}
	c.modules = c.modules[:0]
	for line := range strings.SplitSeq(modInfo, "\r\n") {
		if after, found := strings.CutPrefix(line, "module:"); found {
			m := parseModuleLine(after)
			if m.Name != "" {
				c.modules = append(c.modules, m)
			}
		}
	}

	// --- 命令族级能力（分层探测）---
	// 先清缓存，避免重试路径残留上一轮结果。
	c.hasCuckoo, c.hasCMS, c.hasTopK, c.hasTDigest = false, false, false, false

	// 只有 bf 模块在场才值得探测 CF/CMS/TOPK/TDIGEST：RedisBloom 由单个
	// 模块提供全部命令族，bf 不在场时四族必不可用（valkey-bloom、
	// Redis 8 裸二进制等形态只有 BF.*，探测结果同样是 false，省掉 4 个
	// 往返）。
	//
	// ⚠️ 锁序：本函数由 Probe 在**持有 c.mu** 的路径上调用，绝不能调
	// c.HasModule("bf")——它再次加锁，而 sync.Mutex 不可重入，同 goroutine
	// 自死锁。故此处直接查 c.modules，匹配口径与 HasModule 完全一致
	// （EqualFold 全名）。
	bfLoaded := slices.ContainsFunc(c.modules, func(m moduleInfo) bool {
		return strings.EqualFold(m.Name, "bf")
	})
	if bfLoaded {
		if err := c.probeCommandFamily(ctx); err != nil {
			// 网络/服务端错误：原样返回——**不得把探测失败当作"不支持"
			// 缓存**，否则瞬断会把 CF.* 路径永久降级为 Lua 回退实现；
			// 需要重试由调用方再次 Probe。
			return err
		}
	}

	// 全部探测步骤成功——置位（区分「未探测」与「探测后无模块」，G4）。
	c.probed = true
	return nil
}

// commandExists 用 `COMMAND INFO <cmd>` 判定单条命令在服务端是否存在。
//
// 为什么走 Do 而非 go-redis 的 CommandInfo/Commands：v9.22 的
// Client.CommandInfo 不支持带命令名前缀的子命令查询、也没有带参
// CommandInfo(name) 方法；手工用 NewCommandsInfoCmd 拼 `COMMAND INFO x`
// 会在命令不存在时踩到 nil 元素的协议解析错误（该 Cmd 的读路径按
// "必为数组"假设解析）。Do 返回通用 *Cmd，nil 元素原样落到 []any 中，
// 由 parseCommandInfoResponse 安全判定。
//
// 调用方须处理返回的 error：网络错误 ≠ 命令不存在（见 probeCommandFamily）。
func (c *Capability) commandExists(ctx context.Context, cmd string) (bool, error) {
	val, err := c.rdb.Do(ctx, "COMMAND", "INFO", cmd).Result()
	return parseCommandInfoResponse(val, err)
}

// parseCommandInfoResponse 是 COMMAND INFO 回复的纯解析函数（无 IO，
// 可表驱动单测）。服务端语义：
//   - 命令存在 → 数组，首元素为该命令的信息数组；
//   - 命令不存在 → 数组长度为 1 且首元素为 nil（RESP 空元素），或空数组。
func parseCommandInfoResponse(val any, err error) (bool, error) {
	if err != nil {
		return false, err // 透传：调用方据此区分"探测失败"与"不支持"
	}
	info, ok := val.([]any)
	if !ok {
		// 防御分支：代理/异构服务端回了非数组（状态回复、整数等），
		// 无法据此确认命令存在，按"不存在"处理且不 panic——宁可保守
		// 回退 Lua 实现，也不把命令发往可能不支持它的服务端。
		return false, nil
	}
	if len(info) == 0 || info[0] == nil {
		return false, nil
	}
	return true, nil
}

// probeCommandFamily 逐族确认 CF / CMS / TOPK / TDIGEST 命令是否可用，
// 结果写入 Capability 缓存字段。仅在 probeLocked 判定 bf 在场后调用，
// 处于持锁路径上——只能用 c.rdb.Do（不触碰 Capability 自身的锁）。
//
// 任一命令的探测出现网络/服务端错误即原样返回 error（4 次往返最多
// 阻塞一次探测），调用方不得将其当作 false 缓存。
//
// 已知限制（Cluster 异构，有意不做逐分片探测——YAGNI）：
// `COMMAND INFO` 是无 key 命令，go-redis Cluster 把它路由到**随机节点**，
// 因此异构加载（只有部分节点有 RedisBloom）的集群里，探测结果代表
// 该随机节点而非全局。本库对模块/Lua 能力的一贯前提是"集群各节点
// 配置同构"（见 bloom.go 的分片同构声明），异构集群本就不在支持面内。
func (c *Capability) probeCommandFamily(ctx context.Context) error {
	cuckoo, err := c.commandExists(ctx, "CF.ADD")
	if err != nil {
		return err
	}
	cms, err := c.commandExists(ctx, "CMS.MERGE")
	if err != nil {
		return err
	}
	topk, err := c.commandExists(ctx, "TOPK.ADD")
	if err != nil {
		return err
	}
	tdigest, err := c.commandExists(ctx, "TDIGEST.ADD")
	if err != nil {
		return err
	}

	c.hasCuckoo, c.hasCMS, c.hasTopK, c.hasTDigest = cuckoo, cms, topk, tdigest
	return nil
}

// parseModuleLine parses a module info line like:
//
//	module:name=ReJSON,ver=20000,api=1,filters=0,usedby=[],...
func parseModuleLine(line string) moduleInfo {
	var m moduleInfo
	for part := range strings.SplitSeq(line, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "name":
			m.Name = v
		case "ver":
			m.Version = v
		}
	}
	return m
}

// Probed 报告 Capability 是否已完成一次**全部步骤成功**的探测（Probe/
// probeLocked 无错返回）。与 HasBloom/HasCuckoo 等查询的区别：那些方法
// 在「未探测」与「探测后无模块」两态下同样返回 false，本方法区分二者
// （v0.11.0 G4：工厂模块期望校验据此拦截「未探测即要求模块路径」的
// 静默回退）。部分失败的探测不置位；Refresh 清除置位。纯内存读。
func (c *Capability) Probed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.probed
}

// Version returns the Redis server version string (e.g. "7.2.5").
// 纯内存读：未 Probe 过时返回空串。
func (c *Capability) Version() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// VersionAtLeast returns true if the server version >= minVersion (e.g. "7.4").
// 纯内存读：未 Probe 过（或版本不可解析）时返回 false。
func (c *Capability) VersionAtLeast(minVersion string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.versionSem == nil {
		return false
	}
	constraint, err := version.NewConstraint(">= " + minVersion)
	if err != nil {
		return false
	}
	return constraint.Check(c.versionSem)
}

// HasModule returns true if a module with the given name is loaded.
// Matching is case-insensitive（EqualFold 全名）。
// 纯内存读：未 Probe 过时返回 false。
func (c *Capability) HasModule(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.ContainsFunc(c.modules, func(m moduleInfo) bool {
		return strings.EqualFold(m.Name, name)
	})
}

// HasJSON 报告是否加载了 RedisJSON（ReJSON）模块。
// 纯内存读：未 Probe 过时恒 false。
func (c *Capability) HasJSON() bool { return c.HasModule("ReJSON") }

// HasSearch 报告是否加载了 search（RediSearch）模块。
// 纯内存读：未 Probe 过时恒 false。
func (c *Capability) HasSearch() bool { return c.HasModule("search") }

// HasTimeSeries 报告是否加载了 timeseries 模块。
// 纯内存读：未 Probe 过时恒 false。
func (c *Capability) HasTimeSeries() bool { return c.HasModule("timeseries") }

// HasGraph 报告是否加载了 graph（RedisGraph）模块。
// 纯内存读：未 Probe 过时恒 false。
func (c *Capability) HasGraph() bool { return c.HasModule("graph") }

// HasGears 报告是否加载了 gears 模块。
// 纯内存读：未 Probe 过时恒 false。
func (c *Capability) HasGears() bool { return c.HasModule("gears") }

// HasVectorSet 报告是否加载了 vectorset 模块。
// 纯内存读：未 Probe 过时恒 false。
func (c *Capability) HasVectorSet() bool { return c.HasModule("vectorset") }

// 以上便捷判定的口径与 HasModule 一致（EqualFold 全名匹配真实模块名）；
// 命令前缀 cf/cms/topk/tdigest 不是模块名，对应判定走下方命令族缓存方法。

// HasBloom 判定 BF.* 命令族可用性，口径是 bf 模块在场（INFO MODULES）。
// 不叠加命令族探测：bf 在场 ⇒ BF.* 可用，在 RedisBloom、valkey-bloom、
// Redis 8 内建等形态下均成立（BF.* 是该模块的核心命令族，不存在"模块
// 加载了但 BF. 不可用"的实际形态），无需多付 1 个往返。
// 纯内存读：未 Probe 过时返回 false。
func (c *Capability) HasBloom() bool { return c.HasModule("bf") }

// HasCuckoo 报告 CF.*（布谷鸟）命令族是否可用：读 bf 模块在场时由
// probeCommandFamily 经 `COMMAND INFO CF.ADD` 确认的缓存；bf 不在场必为
// false。纯内存读（加锁读字段）：未 Probe 过时恒 false。
func (c *Capability) HasCuckoo() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasCuckoo
}

// HasCMS 报告 CMS.*（计数-最小 sketch）命令族是否可用，判定与读取口径
// 同 HasCuckoo。纯内存读：未 Probe 过时恒 false。
func (c *Capability) HasCMS() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasCMS
}

// HasTopK 报告 TOPK.* 命令族是否可用，判定与读取口径同 HasCuckoo。
// 纯内存读：未 Probe 过时恒 false。
func (c *Capability) HasTopK() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasTopK
}

// HasTDigest 报告 TDIGEST.* 命令族是否可用，判定与读取口径同 HasCuckoo。
// 纯内存读：未 Probe 过时恒 false。
func (c *Capability) HasTDigest() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasTDigest
}
