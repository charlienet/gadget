package redis

import (
	"context"
	"strings"
	"sync"

	"github.com/hashicorp/go-version"
)

// Capability provides cached information about the Redis server's version
// and loaded modules. It is lazily populated on first access and can be
// refreshed via Probe or Refresh.
type Capability struct {
	mu    sync.Mutex
	rdb   *redisClient
	ready bool

	version    string
	versionSem *version.Version
	modules    []moduleInfo

	// 命令族级能力缓存（v0.7.0 起新增）：由 probeCommandFamily 经
	// `COMMAND INFO <族>.<命令>` 逐族真实确认，仅在 bf 模块在场时探测，
	// 其余情况恒 false。**不得**再用模块名判定——INFO MODULES 里的模块名
	// 是 bf/cb/RedisBloom 等加载名，而 cf/cms/topk/tdigest 只是命令前缀，
	// 按前缀查模块名恒 false（历史缺陷）。
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
// 探测失败（如服务器不可达）时返回真实错误，且不标记缓存就绪，
// 后续访问会重新探测（见 ensureLoaded）。
func (c *Capability) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.probeLocked(ctx)
}

// Refresh forces a re-probe on the next access (discards cached data).
func (c *Capability) Refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false
}

func (c *Capability) probeLocked(ctx context.Context) error {
	// 注意：探测全部成功（INFO 命令执行成功）才置 ready=true；
	// 任一探测失败直接返回错误，ready 保持 false，后续访问会重试（见 ensureLoaded）。

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
	// c.HasModule("bf")——它先 ensureLoaded()→Probe()→再次 Lock，而
	// sync.Mutex 不可重入，同 goroutine 自死锁。故此处直接遍历
	// c.modules 判等，匹配口径与 HasModule 完全一致（EqualFold 全名）。
	bfLoaded := false
	for _, m := range c.modules {
		if strings.EqualFold(m.Name, "bf") {
			bfLoaded = true
			break
		}
	}
	if bfLoaded {
		if err := c.probeCommandFamily(ctx); err != nil {
			// 网络/服务端错误：原样返回、ready 保持 false（与本函数
			// 现有 INFO 失败语义一致）——**不得把探测失败当作"不支持"
			// 缓存**，否则瞬断会把 CF.* 路径永久降级为 Lua 回退实现。
			return err
		}
	}

	// INFO 与（bf 在场时的）命令族探测均成功，才标记缓存就绪
	c.ready = true
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

// ensureLoaded probes once on first access.
func (c *Capability) ensureLoaded() {
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	if !ready {
		// 惰性探测：失败静默忽略（错误详情可通过主动调用 Probe 获取），
		// 且 ready 保持 false，后续访问会重试，避免探测失败被永久缓存。
		_ = c.Probe(context.Background())
	}
}

// Version returns the Redis server version string (e.g. "7.2.5").
func (c *Capability) Version() string {
	c.ensureLoaded()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// VersionAtLeast returns true if the server version >= minVersion (e.g. "7.4").
func (c *Capability) VersionAtLeast(minVersion string) bool {
	c.ensureLoaded()
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
// Matching is case-insensitive.
func (c *Capability) HasModule(name string) bool {
	c.ensureLoaded()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.modules {
		if strings.EqualFold(m.Name, name) {
			return true
		}
	}
	return false
}

// Convenience module checks.
//
// 命令前缀 cf/cms/topk/tdigest 不是模块名，对应判定走 probeCommandFamily
// 的探测缓存（见下方 HasCuckoo 等）；HasModule 只适用于真实模块名。
func (c *Capability) HasJSON() bool       { return c.HasModule("ReJSON") }
func (c *Capability) HasSearch() bool     { return c.HasModule("search") }
func (c *Capability) HasTimeSeries() bool { return c.HasModule("timeseries") }
func (c *Capability) HasGraph() bool      { return c.HasModule("graph") }
func (c *Capability) HasGears() bool      { return c.HasModule("gears") }
func (c *Capability) HasVectorSet() bool  { return c.HasModule("vectorset") }

// HasBloom 判定 BF.* 命令族可用性，口径是 bf 模块在场（INFO MODULES）。
// 不叠加命令族探测：bf 在场 ⇒ BF.* 可用，在 RedisBloom、valkey-bloom、
// Redis 8 内建等形态下均成立（BF.* 是该模块的核心命令族，不存在"模块
// 加载了但 BF. 不可用"的实际形态），无需多付 1 个往返。
func (c *Capability) HasBloom() bool { return c.HasModule("bf") }

// HasCuckoo/HasCMS/HasTopK/HasTDigest 判定对应命令族是否可用：读 bf 在
// 场时由 probeCommandFamily 经 `COMMAND INFO` 逐族确认的缓存（v0.7.0 起；
// 此前误按模块名 "cf"/"cms"/"topk"/"tdigest" 查 INFO MODULES，恒 false）。
// bf 不在场时四者必为 false，无需探测。
//
// 锁结构与 HasModule 一致：先 ensureLoaded（内部会加锁探测，故**不可**
// 在持锁路径调用），再加锁读字段。
func (c *Capability) HasCuckoo() bool {
	c.ensureLoaded()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasCuckoo
}

func (c *Capability) HasCMS() bool {
	c.ensureLoaded()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasCMS
}

func (c *Capability) HasTopK() bool {
	c.ensureLoaded()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasTopK
}

func (c *Capability) HasTDigest() bool {
	c.ensureLoaded()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasTDigest
}
