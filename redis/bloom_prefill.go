package redis

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Bloom/布谷鸟 预填充门控与降级：Redis 权威状态键 + 本地 coordinator 同步 +
// 数据面分派（bloom 装饰器见 bloom_prefill_filter.go，cuckoo 门面分派见
// cuckoo.go）。状态机与同步机制为 bloom/cuckoo 共用核心。
//
// 键协议（Cluster 同 slot 硬要求）：状态键/计数键基于 tagbase——base 在
// 首个 '{' 之后含非空 '}' 时视为自带 hash tag 原样使用，否则统一包裹
// {base}；两键同 hash tag → 同 slot，acquire/ready/fail 三个 Lua 同时
// 操作两键（KEYS=[state,failn]）。前缀由 renameHook 统一添加（EVAL 按
// numkeys 改写），代码只传业务口径键名；数据键（<base>#<idx> / <base>）
// 不加 hash tag、不动 bloomSharder。
//
// 状态机（T1-T10）：Uninitialized/Building/Failed/Ready 相位迁移由三个
// Lua 原子仲裁，本地 atomic 相位仅作热路径零 RTT 判定与 stale 降级。
const (
	// prefillStateReady 等：权威状态键三值；键缺失 = Uninitialized。
	prefillStateReady    = "ready"
	prefillStateBuilding = "building"
	prefillStateFail     = "fail"

	// prefillBuildingSlack 是 building TTL 相对 RebuildTimeout 的固定最小
	// 裕量；实际裕量 = max(prefillBuildingSlack, 2×syncInterval+5s)，
	// 见 prefillBuildingTTL（大 syncInterval 下 Δ 会吃穿固定裕量）。
	prefillBuildingSlack = 10 * time.Second

	// prefillStatusTimeout 是状态读写（sync/State/状态脚本）的独立超时，
	// 与调用方 ctx 解耦（失败路径写 fail 用 WithoutCancel 派生 + 本超时）。
	prefillStatusTimeout = 5 * time.Second

	// prefillFailnTTLSeconds 是连续失败计数键的兜底 EXPIRE（24h）。
	prefillFailnTTLSeconds = 86400

	// prefillProbeTimeoutCap 是单轮有效性探测（probeFn 取样 + ExistsMulti
	// 查询两阶段）的超时上限：min(probeInterval, 本值)；不约束失效触发的
	// 重建——重建预算为 rebuildTimeout，见 probeOnce e。探测基于 baseCtx
	// 派生——单请求取消不影响它，Close 必须能停。
	prefillProbeTimeoutCap = 30 * time.Second
)

// PrefillPhase 是预填充相位（本地缓存与权威状态的共同口径）。
type PrefillPhase uint32

const (
	// PrefillUninitialized 状态键缺失：尚未预填充（可被任一 force 抢占）。
	PrefillUninitialized PrefillPhase = iota
	// PrefillBuilding 有执行者持锁重建中（任意 acquire 返回 0）。
	PrefillBuilding
	// PrefillFailed 上次重建失败，fail 键带退避 TTL。
	PrefillFailed
	// PrefillReady 预填充完成，数据面恢复真实查询。
	PrefillReady
)

// String 返回相位名（诊断/日志用）。
func (p PrefillPhase) String() string {
	switch p {
	case PrefillUninitialized:
		return "uninitialized"
	case PrefillBuilding:
		return "building"
	case PrefillFailed:
		return "failed"
	case PrefillReady:
		return "ready"
	default:
		return "unknown"
	}
}

// PrefillFunc 是应用回灌函数：在清空重建后的过滤器上从权威数据源回灌
// 数据。契约（见 WithPrefill/WithCuckooPrefill godoc）：幂等可重试、以
// 权威数据源为扫描基准、响应 ctx 取消（预算 RebuildTimeout）；panic 由库
// recover 转 error 走 fail 路径。
type PrefillFunc func(ctx context.Context, ingest PrefillIngest) error

// LivenessFunc 返回肯定样本：返回值必须是数据源中确定存在、且按契约
// 已回灌进过滤器的条目。样本错误（含已删除项未剔除）会导致周期性
// 误触发重建——这是应用侧数据契约 bug 的可见化，库不做连续次数容忍。
type LivenessFunc func(ctx context.Context) ([]any, error)

// PrefillIngest 是回灌期间的数据写入面：直连 inner 实现（不经门面/
// 装饰器降级分派、不触发惰性重建）。
type PrefillIngest interface {
	// Add 写入单条，语义同宿主过滤器的 Add。
	Add(ctx context.Context, item any) (bool, error)
	// AddMulti 批量写入，语义同宿主过滤器的 AddMulti。
	AddMulti(ctx context.Context, items ...any) ([]bool, error)
}

// prefillIngest 是 PrefillIngest 的直连接口实现（bypass 门面/装饰器）。
type prefillIngest struct {
	inner prefillInner
}

func (i *prefillIngest) Add(ctx context.Context, item any) (bool, error) {
	return i.inner.Add(ctx, item)
}

func (i *prefillIngest) AddMulti(ctx context.Context, items ...any) ([]bool, error) {
	return i.inner.AddMulti(ctx, items...)
}

var (
	// ErrRebuildInProgress 表示权威状态为 Building，本次抢占失败：
	// 显式 Reset 立即返回，不排队不等锁。
	ErrRebuildInProgress = errors.New("redis: bloom prefill rebuild in progress")
	// ErrRebuildTimeout 表示重建执行超出 RebuildTimeout 预算（包装底层
	// context.DeadlineExceeded，errors.Is 双向可感知）。
	ErrRebuildTimeout = errors.New("redis: bloom prefill rebuild timeout")
	// ErrPrefillDisabled 表示过滤器未启用 WithPrefill/WithCuckooPrefill，
	// State 无预填充语义可执行。
	ErrPrefillDisabled = errors.New("redis: bloom prefill disabled")
)

// prefillConfig 是 WithPrefill 的配置（嵌入 bloomConfig）。
// 有效性探测字段平铺（规格：不加独立配置子结构）。
type prefillConfig struct {
	enabled        bool          // 是否启用门控（WithPrefill(fn!=nil) 置位）
	fn             PrefillFunc   // 应用回灌函数
	rebuildTimeout time.Duration // 单次重建预算，默认 5min；buildingTTL=该值+max(10s,2×syncInterval+5s)
	retryInitial   time.Duration // 退避初始步长，默认 5s
	retryMax       time.Duration // 退避上限，默认 10min（multiplier 固定 2）
	syncInterval   time.Duration // 本地状态同步间隔，默认 1s；Δ=2×、stale=2×
	probeInterval  time.Duration // 有效性探测间隔（<=0=不启用，见 WithLivenessProbe）
	probeFn        LivenessFunc  // 有效性探测取样函数（nil=不启用）
}

// PrefillOption 配置预填充门控（WithRebuildTimeout/WithRetryBackoff/
// WithSyncInterval/WithLivenessProbe 返回值），非法值静默忽略。
type PrefillOption func(*prefillConfig)

// WithLivenessProbe 配置有效性探测（liveness probe）：按 interval 从 fn
// 取"数据源确定存在且已回灌"的肯定样本，直查 inner（绕开门面/装饰器
// 降级分派与 FailPolicy 兜底）；bloom 无假阴——任一样本查不到即数据已
// 丢的确定性证据，触发 force=1 重建（与 Reset 同路，多实例锁仲裁，
// 可经 State 观察相位；重建不新增钩子）。非法值（interval<=0 或
// fn==nil）静默忽略 = 不启用探测，对齐既有 option 惯例。
//
//   - 探测间隔 = 触发重建的天然限频下界（下轮按间隔再探测，同一轮不重查）；
//   - 只在本实例本地相位 Ready 且新鲜时探测（降级期 Exists 恒 true 无
//     判别意义，且避免重建期误触发）；
//   - 与 FailPolicy 正交：探测错误（probeFn err/panic、ExistsMulti 底层
//     错误）只跳过本轮，不构成失效证据；
//   - 与 G1 完整性校验（IntegrityProbe，搭车 syncOnce）互补：完整性
//     校验覆盖键缺失/异类型占用/布局不符，但存在盲区——键在、类型对、
//     内容被清空（DEL 后空 RESERVE 重建）无法由键形态检出，该形态由
//     本选项的肯定样本探测承接，建议两者同时启用；
//   - 数据源为空/样本为空时探测静默跳过本轮（不判失效），空数据源下
//     ready 保持为预期行为；
//   - LivenessFunc 契约见其 godoc——样本错误会导致周期性误触发重建，
//     属应用侧数据契约 bug 的可见化，库不做连续次数容忍；
//   - PrefillOption 为 bloom/cuckoo 共用，本选项两处自动可用：
//     bloom 经 WithPrefill(fn, WithLivenessProbe(...)) 的 opts 传入，
//     cuckoo 经 WithCuckooPrefill(fn, WithLivenessProbe(...)) 的 opts
//     传入（不新增 WithCuckooXxx 形态）。
func WithLivenessProbe(interval time.Duration, fn LivenessFunc) PrefillOption {
	return func(c *prefillConfig) {
		if interval > 0 && fn != nil {
			c.probeInterval = interval
			c.probeFn = fn
		}
	}
}

func defaultPrefillConfig() prefillConfig {
	return prefillConfig{
		rebuildTimeout: 5 * time.Minute,
		retryInitial:   5 * time.Second,
		retryMax:       10 * time.Minute,
		syncInterval:   time.Second,
	}
}

// --- 键协议（规格 §1） ---

// prefillTagBase 计算状态键前缀：base 自带有效 hash tag（首个 '{' 之后
// 存在非空内容的 '}'，即 Redis 规范的 {tag} 形态）时原样使用，否则
// （无 '{'、未闭合 x{y、空 tag {}）统一包裹 {base}——保证状态键/计数键
// 同 hash tag → Cluster 同 slot。未闭合形态若原样使用，两键将按整键算
// slot 而不同 slot → 双键 EVAL CROSSSLOT。数据键不参与本变换。
func prefillTagBase(base string) string {
	if i := strings.IndexByte(base, '{'); i >= 0 {
		// base[i+1:] 跳过 '{'：'}' 下标 0 为空 tag（{}），<0 未闭合，
		// >0 才是闭合且非空的有效 tag。
		if j := strings.IndexByte(base[i+1:], '}'); j > 0 {
			return base
		}
	}
	return "{" + base + "}"
}

// prefillStateKeys 返回状态键与计数键（业务口径，前缀由 renameHook 统一
// 添加）：<tagbase>:__prefill 与 <tagbase>:__prefill:failn。
func prefillStateKeys(base string) (stateKey, failnKey string) {
	tb := prefillTagBase(base)
	return tb + ":__prefill", tb + ":__prefill:failn"
}

// phaseFromValue 映射权威状态键值 → 相位；未知值按 Uninitialized（可被
// 重新抢占，fail-safe）。
func phaseFromValue(v string) PrefillPhase {
	switch v {
	case prefillStateReady:
		return PrefillReady
	case prefillStateBuilding:
		return PrefillBuilding
	case prefillStateFail:
		return PrefillFailed
	default:
		return PrefillUninitialized
	}
}

// backoffDuration 计算第 n 次连续失败的退避：
// min(initial × 2^(n-1), max)。n<1 按 1；乘法前以 max/2 封顶防溢出。
func backoffDuration(n int64, initial, max time.Duration) time.Duration {
	if n < 1 {
		n = 1
	}
	if initial <= 0 {
		initial = defaultPrefillConfig().retryInitial
	}
	if max <= 0 {
		max = defaultPrefillConfig().retryMax
	}
	d := initial
	for i := int64(1); i < n; i++ {
		if d >= max/2 {
			return max
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}

// prefillBuildingTTL 计算抢占时写入的 building TTL：
// rebuildTimeout + max(prefillBuildingSlack, 2×syncInterval+5s)。
// 裕量必须覆盖 Δ=2×syncInterval 等待与状态写往返——WithSyncInterval 放大
// 时固定 10s 裕量会被 Δ 吃穿，building 提前过期将打开双执行者窗口。
func prefillBuildingTTL(rebuildTimeout, syncInterval time.Duration) time.Duration {
	margin := prefillBuildingSlack
	if d := 2*syncInterval + 5*time.Second; d > margin {
		margin = d
	}
	// N5：病态极大 rebuildTimeout 下防加法回绕为负（负 PX 会让 acquire
	// Lua 报错）——饱和钳位到 Duration 上限；正常配置不走此分支。
	if margin > 0 && rebuildTimeout > time.Duration(1<<63-1)-margin {
		return time.Duration(1<<63 - 1)
	}
	return rebuildTimeout + margin
}

// --- Lua 脚本（规格 §3；KEYS[1]=状态键 KEYS[2]=failn 键，两键同 slot） ---
// 脚本内只引用 KEYS[i]/ARGV，不硬拼键名；backoff 时长由客户端算好以
// 毫秒 ARGV 传入（Lua 不做算术）。

// prefillAcquireScript 抢占持锁：building 一律返回 0（T5）；force=0 时
// Ready 不抢占（T2）、fail 且 PTTL>0 退避未满不抢占（T3）；否则置
// building（PX=ARGV[1] buildingTTL 毫秒）。force=1 成功时 DEL failn
// 归零连续失败计数（T4，含从 Ready/Uninitialized 抢占）。键缺失 GET 为
// false ≠ building → 可抢（T1）。
var prefillAcquireScript = goredis.NewScript(`
	local v = redis.call('GET', KEYS[1])
	if v == 'building' then
		return 0
	end
	if ARGV[2] == '0' then
		if v == 'ready' then
			return 0
		end
		if v == 'fail' and redis.call('PTTL', KEYS[1]) > 0 then
			return 0
		end
	end
	redis.call('SET', KEYS[1], 'building', 'PX', ARGV[1])
	if ARGV[2] == '1' then
		redis.call('DEL', KEYS[2])
	end
	return 1
`)

// prefillReadyScript 完成置 ready（T6）：仅当当前值仍=building 才 SET
// ready（无 TTL 持久，SET 覆盖清除原 PX）+ DEL failn 返回 1；否则返回 0
// 不改状态（状态漂移由调用方忽略并在下轮同步纠正）。
var prefillReadyScript = goredis.NewScript(`
	if redis.call('GET', KEYS[1]) == 'building' then
		redis.call('SET', KEYS[1], 'ready')
		redis.call('DEL', KEYS[2])
		return 1
	end
	return 0
`)

// prefillFailScript 失败记账（T7）：INCR failn 得 n → EXPIRE failn 24h
// → SET state fail PX ARGV[1]（客户端算好的 backoff 毫秒，无条件覆盖）；
// 返回 n。
var prefillFailScript = goredis.NewScript(`
	local n = redis.call('INCR', KEYS[2])
	redis.call('EXPIRE', KEYS[2], 86400)
	redis.call('SET', KEYS[1], 'fail', 'PX', ARGV[1])
	return n
`)

// prefillLocal 是本地缓存的相位快照（phase + updatedAt 一体更新）。
// 注意：syncOnce 的错误观测字段不并入本结构——storeLocal 有多个调用点
// （syncOnce×2、finish×2），并入会使每次相位刷新连带清零未携带字段；
// 错误由独立的 errSnapshot 指针承载，见 prefillCoordinator.syncErr。
type prefillLocal struct {
	phase     PrefillPhase
	updatedAt time.Time
}

// errSnapshot 承载 syncOnce 路径的末次底层错误及其时间。err 为 nil 表示
// 最近一拍同步全 IO 成功（含 NotFound——键缺失是正常相位非错误），at 仍
// 保留末次错误时间作诊断（清零口径只清 err 不清 at）。
type errSnapshot struct {
	err error
	at  time.Time
}

// PhaseInfo 是 BloomFilter.Phase / CuckooFilter.Phase 返回的本地相位快照
// 视图（零 RTT 纯内存读，由 coordinator 的两枚独立 atomic 指针组装）。
//
// 分工线：本地视图（Phase）用于放行分流——读的是本实例缓存，非权威，
// fail-safe 方向使用（Fresh=false 按降级理解）；权威读 State(ctx)（1 RTT
// 直接 GET 状态键）用于不可逆决策（重建、清理、运维判定等以权威为准）。
//
// 多实例滞后声明：本快照经后台 ticker 同步，滞后权威状态最多约
// 1×syncInterval + 1 个 RTT；Fresh=false 时相位不可信，须按降级态
// （数据面恒放行）理解，而非"权威一定不是 Ready"。
//
// 组装口径：Phase/UpdatedAt/Fresh 取自相位快照指针（freshOf 单源判定，
// 与热路径 readyFresh 同口径）；LastSyncErr/LastSyncErrAt 取自错误快照
// 指针。两枚指针间无原子撕裂保证——Phase() 组合读不保证跨字段同一瞬间
// 一致（如相位刚刷新、错误指针尚是上一拍的），但各字段自带时间戳、
// 均为诊断/分流用途，语义可接受。
type PhaseInfo struct {
	// Phase 是本地缓存相位（非权威，滞后见类型 godoc）。
	Phase PrefillPhase
	// UpdatedAt 是本地快照最近一次成功同步（或就地重建 storeLocal）的
	// 时间；零值表示构造后从未成功同步（首拍即视为超龄降级）。
	UpdatedAt time.Time
	// Fresh 报告快照是否可作放行依据：相位 Ready 且未超龄
	// （age ≤ 2×syncInterval），与装饰器降级分派的 readyFresh 同口径
	// （同一判定源组装，非复制表达式）。
	Fresh bool
	// LastSyncErr 是最近一次后台 syncOnce 路径的底层错误，原样存储
	// 不包装（errors.Is / IsUnavailable / IsNotFound 分类能力保留）。
	// 仅代表后台同步路径错误：Ready 透传态下数据面（Exists 等）的真实
	// 错误仍原样返回调用方，两个观测点互不干扰。nil = 最近一拍全 IO
	// 成功。用 IsUnavailable/IsNotFound 对本值分类。
	LastSyncErr error
	// LastSyncErrAt 是 LastSyncErr 的记录时间；LastSyncErr 已清 nil 时
	// 本值保留末次错误时间作诊断（不清零）。零值 = 从未记录过错误。
	LastSyncErrAt time.Time
}

// prefillInner 是 prefill 核心对宿主过滤器的最小依赖：清空重建骨架
// （Reset）、回灌写入（Add/AddMulti）、有效性探测直查（Exists/
// ExistsMulti——绕开门面/装饰器降级分派与 FailPolicy 兜底，拿到原始
// 错误与真实值）与完整性校验（IntegrityProbe——G1 键存在性/布局校验）。
// BloomFilter 与 cuckooFilterImpl 均满足——本文件的状态机/ticker/退避
// 为两者共用。
type prefillInner interface {
	Reset(ctx context.Context) error
	Add(ctx context.Context, item any) (bool, error)
	AddMulti(ctx context.Context, items ...any) ([]bool, error)
	Exists(ctx context.Context, item any) (bool, error)
	ExistsMulti(ctx context.Context, items ...any) ([]bool, error)
	// IntegrityProbe 三态校验宿主过滤器物理键的存在性/布局（裸命令，
	// 禁 Lua——判据见各 impl）：
	//   - ok=true, err=nil → 完整性证据充分（键在且形态正确）；
	//   - ok=false, err=nil → 失效证据（键缺失/异类型占用/布局不符）；
	//   - err != nil → 传输级错误（Unavailable 等），本轮无结论——
	//     严禁折叠成 invalid（网络抖动不得引发重建）。
	//
	// 覆盖：键缺失、异类型占用、（bitmap）布局长度不符；已知盲区：
	// 键在、类型对、但内容被清空（DEL 后空 RESERVE 重建）——该形态由
	// WithLivenessProbe 肯定样本探测承接，建议同时启用。
	//
	// 成本：每拍 O(1) 命令 ×分片数（TYPE/STRLEN），实例数×分片数放大；
	// 检出窗口 ≤ syncInterval + 1 RTT（调大 WithSyncInterval 线性变差）。
	// 本方法只读证据、不写任何状态。
	IntegrityProbe(ctx context.Context) (bool, error)
}

// integrityTypeInvalid 是 bf/cf 模块键 TYPE 判据的负面清单：返回 true
// 当且仅当 t 为 Redis 核心值类型名（含 none——两路径构造即建键，缺失
// 即失效证据）。清单外的任意类型名（实测 BF=MBbloom--、CF=MBbloomCF，
// 及未来模块内部类型名变更）按肯定证据处理——**禁硬编码模块类型名**
// （ISSUE-103：模块内部命名属上游实现细节，硬编码会在其变更时静默失效）。
func integrityTypeInvalid(t string) bool {
	switch t {
	case "none", "string", "hash", "list", "set", "zset", "stream":
		return true
	}
	return false
}

// integrityTypeProbe 以单次 pipeline 批量 TYPE keys（裸命令，禁 Lua——
// ISSUE-105；分片 n 键 1 往返）执行 bf/cf 共用判据：任一键落入负面
// 清单 → (false, nil)；TYPE 对任意键形态都正常回答（none/类型名），
// Exec 出现的 err 必为传输级 → 原样返回（无结论）。
func integrityTypeProbe(ctx context.Context, c *redisClient, keys []string) (bool, error) {
	pipe := c.Pipeline()
	cmds := make([]*goredis.StatusCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.Type(ctx, key)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}
	for _, cmd := range cmds {
		if integrityTypeInvalid(cmd.Val()) {
			return false, nil
		}
	}
	return true, nil
}

// 编译期护栏（ISSUE-204）：bf/cf 共用路径与装饰器的数据面宿主实现必须
// 满足 prefillInner（含 IntegrityProbe）——接口漂移在编译期暴露。
var (
	_ prefillInner = (*bfCmdImpl)(nil)
	_ prefillInner = (*bitmapImpl)(nil)
)

// prefillCoordinator 是每实例每 filter 的本地协调器：后台 ticker 同步
// 权威状态、热路径惰性触发、显式/自动重建执行与就地相位更新。
// 注册进 redisClient 的 closeState 级联表（GracefulClose 停 ticker）。
type prefillCoordinator struct {
	rdb      *redisClient
	inner    prefillInner
	fn       PrefillFunc
	cfg      prefillConfig
	stateKey string
	failnKey string

	phase         atomic.Pointer[prefillLocal] // 本地相位快照（phase+updatedAt 原子一体）
	syncErr       atomic.Pointer[errSnapshot]  // syncOnce 末次错误（独立指针：storeLocal 多调用点不连带清零）
	inFlight      atomic.Bool                  // 本地重建防重入（force=0 触发方 CAS 持有）
	nextTriggerAt atomic.Int64                 // Failed 本地冷却截止（unixnano；仅近似，权威由 fail TTL 把守）
	acquireTries  atomic.Int64                 // acquire 尝试计数（诊断/测试判别探针）

	baseCtx    context.Context // 生命周期 ctx：Close 时 cancel，传导取消在飞 run
	baseCancel context.CancelFunc

	stopCh   chan struct{}
	stopOnce sync.Once
}

// newPrefillCoordinator 组装协调器并注册 closeState 级联（关闭即停
// ticker 并取消在飞 run）；启动后台同步 loop。
func newPrefillCoordinator(rdb *redisClient, inner prefillInner, base string, cfg prefillConfig) *prefillCoordinator {
	stateKey, failnKey := prefillStateKeys(base)
	baseCtx, baseCancel := context.WithCancel(context.Background())
	c := &prefillCoordinator{
		rdb:        rdb,
		inner:      inner,
		fn:         cfg.fn,
		cfg:        cfg,
		stateKey:   stateKey,
		failnKey:   failnKey,
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
		stopCh:     make(chan struct{}),
	}
	// 初值 updatedAt 置零：构造即视为未就绪（age 恒超 stale 阈值，
	// 首请求即降级+可触发，规格 §4）。
	c.phase.Store(&prefillLocal{phase: PrefillUninitialized, updatedAt: time.Time{}})
	rdb.registerClose(c.Close)
	go c.loop()
	go c.probeLoop() // 未启用探测（interval<=0 或 fn==nil）时立即 return，零开销
	return c
}

// stopLoop 只停止后台同步 loop（幂等），不取消在飞 run——测试构造后
// 手动驱动时序用；生产释放走 Close。
func (c *prefillCoordinator) stopLoop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

// Close 完整释放（幂等）：停后台同步 loop 并 cancel 生命周期 ctx，传导
// 取消所有在飞 run（Δ 等待/Reset/fn 随之中断，finish 走 Canceled 禁写
// 分支不写状态，building TTL 自愈）；经 closeState 级联或装饰器 Close
// 调用。
func (c *prefillCoordinator) Close() {
	c.stopLoop()
	c.baseCancel() // context cancel 幂等，重复调用安全
}

// storeLocal 原子一体更新本地相位与更新时间。
func (c *prefillCoordinator) storeLocal(p PrefillPhase) {
	c.phase.Store(&prefillLocal{phase: p, updatedAt: time.Now()})
}

// loadLocal 读取本地相位快照（nil 防御为 Uninitialized 零值）。
func (c *prefillCoordinator) loadLocal() prefillLocal {
	if s := c.phase.Load(); s != nil {
		return *s
	}
	return prefillLocal{phase: PrefillUninitialized}
}

// readyFresh 报告本地相位是否为新鲜 Ready：非 Ready 或 age>2×syncInterval
// 均按非 Ready（stale fail-safe，T10）。判定逻辑单源在 freshOf（与
// PhaseInfo.Fresh 共用，防两处口径漂移）。
func (c *prefillCoordinator) readyFresh() bool {
	return c.freshOf(c.loadLocal())
}

// freshOf 报告给定快照能否作放行依据：phase==Ready 且未超龄（isStale
// 单源判定）。热路径降级分派（readyFresh）与 Phase() 本地视图
// （phaseInfo 组装）都经此，唯一口径。
func (c *prefillCoordinator) freshOf(s prefillLocal) bool {
	if s.phase != PrefillReady {
		return false
	}
	return !c.isStale(s)
}

// phaseInfo 组合两枚 atomic 指针组装 PhaseInfo：相位快照经 loadLocal +
// freshOf（与热路径同口径），错误快照独立读取。跨指针无原子撕裂保证
// （见 PhaseInfo godoc），各字段自带时间戳，语义可接受。
func (c *prefillCoordinator) phaseInfo() PhaseInfo {
	s := c.loadLocal()
	info := PhaseInfo{
		Phase:     s.phase,
		UpdatedAt: s.updatedAt,
		Fresh:     c.freshOf(s),
	}
	if e := c.syncErr.Load(); e != nil {
		info.LastSyncErr = e.err
		info.LastSyncErrAt = e.at
	}
	return info
}

// isStale 报告本地相位快照是否超龄（age>2×syncInterval，T10）。
func (c *prefillCoordinator) isStale(s prefillLocal) bool {
	return time.Since(s.updatedAt) > 2*c.cfg.syncInterval
}

// loop 后台同步：启动先随机 sleep [0, syncInterval)（jitter 防惊群），
// 之后每 syncInterval 同步一次；stopCh 关闭或 client 已 GracefulClose 即
// 自退出（T4 生命周期）。
func (c *prefillCoordinator) loop() {
	syncI := c.cfg.syncInterval
	if syncI <= 0 {
		return
	}
	jitter := time.Duration(rand.Int64N(int64(syncI)))
	jt := time.NewTimer(jitter)
	select {
	case <-c.stopCh:
		jt.Stop()
		return
	case <-jt.C:
	}
	tk := time.NewTicker(syncI)
	defer tk.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-tk.C:
			if c.rdb.isClosed() {
				return
			}
			c.syncOnce()
		}
	}
}

// probeLoop 有效性探测后台循环：与 loop 并存、共用 stopCh（stopLoop/
// Close 天然停，无需改 redis.go 级联）。未启用（probeInterval<=0 或
// probeFn==nil）直接 return——不空转、零开销。首拍 jitter [0, interval)
// 对齐 loop 风格（防多实例同拍探测惊群），之后每 interval 一拍。
// 探测间隔 = 触发重建的天然限频下界（见 WithLivenessProbe godoc）。
func (c *prefillCoordinator) probeLoop() {
	iv := c.cfg.probeInterval
	if iv <= 0 || c.cfg.probeFn == nil {
		return // 未启用探测
	}
	jitter := time.Duration(rand.Int64N(int64(iv)))
	jt := time.NewTimer(jitter)
	select {
	case <-c.stopCh:
		jt.Stop()
		return
	case <-jt.C:
	}
	tk := time.NewTicker(iv)
	defer tk.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-tk.C:
			if c.rdb.isClosed() {
				return
			}
			c.probeOnce(iv)
		}
	}
}

// probeOnce 执行单轮有效性探测（每轮流程见 WithLivenessProbe godoc）：
//
//	a) 本地相位非新鲜 Ready → 本轮跳过（降级期 Exists 恒 true 无判别
//	   意义，且避免重建期误触发）；
//	b) 探测 ctx 派生：baseCtx + timeout=min(interval, 30s)——仅覆盖 c/d
//	   两阶段（取样+查询），不约束 e 触发的重建；探测不可被单请求取消，
//	   但 Close 必须能停（baseCtx cancel）；
//	c) probeFn 取样：err / panic（recover 静默跳过，包内无 logger 不
//	   记录）/ 空样本 → 本轮结束，不构成失效；
//	d) inner.ExistsMulti 直查真实数据面（callExistsMulti 统一 recover：
//	   坏样本经 writer 进入 inner 的 panic 不得崩探测 goroutine）：err /
//	   panic → 本轮结束（错误≠失效：FailClosed 兜底 false、网络错误都
//	   可能让结果不可信）；全 true → 健康，结束；
//	e) 任一 false → 失效（bloom 无假阴 = 确定性证据）：以独立 run ctx
//	   执行 run(force=1)——WithoutCancel(ctx) 剥离探测 deadline/cancel，
//	   按 RebuildTimeout 派生（与 triggerLazy 同口径，重建预算不受
//	   probeInterval 反向耦合）；Close 仍经 run 内 AfterFunc(baseCtx)
//	   合并取消。与显式 Reset 同语义（不经 inFlight CAS，互斥由权威
//	   acquire 仲裁）；ErrRebuildInProgress 静默忽略（已有其它实例在
//	   重建），其余错误按包内惯例 `_ =` 记录性忽略——探测 goroutine 不
//	   向上传播。不在同一轮重查，下轮按间隔再探测。
func (c *prefillCoordinator) probeOnce(interval time.Duration) {
	if !c.readyFresh() {
		return // a
	}
	timeout := interval
	if timeout > prefillProbeTimeoutCap {
		timeout = prefillProbeTimeoutCap
	}
	ctx, cancel := context.WithTimeout(c.baseCtx, timeout) // b
	defer cancel()

	// c：取样（err / panic / 空样本一律跳过本轮）
	samples, ok := c.callProbeFn(ctx)
	if !ok || len(samples) == 0 {
		return
	}

	// d：直查 inner（不经门面/装饰器降级分派与 FailPolicy 兜底）
	hits, ok := c.callExistsMulti(ctx, samples)
	if !ok {
		return // 错误/panic/长度不符 → 本轮跳过（错误≠失效）
	}
	alive := true
	for _, h := range hits {
		if !h {
			alive = false
			break
		}
	}
	if alive {
		return // 全 true → 健康
	}

	// e：任一 false → 失效 → force 重建（与 Reset 同路）；run 预算独立
	// 于探测 ctx（RebuildTimeout，对齐 triggerLazy 口径）
	runCtx, runCancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.rebuildTimeout)
	defer runCancel()
	_ = c.run(runCtx, 1)
}

// callProbeFn 执行应用取样并 panic recover：返回 (samples, true) 当且
// 仅当取样成功且非空前置由调用方判定；err/panic → (nil, false)，静默
// 跳过本轮（不 log：包内无 logger，对齐既有 recover 静默风格）。
func (c *prefillCoordinator) callProbeFn(ctx context.Context) (samples []any, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			samples, ok = nil, false
		}
	}()
	s, err := c.cfg.probeFn(ctx)
	if err != nil {
		return nil, false
	}
	return s, true
}

// callExistsMulti 执行 inner.ExistsMulti 并 panic recover：应用坏样本经
// writer 进入 inner 后的 panic 不在 callProbeFn 的 recover 范围，须在此
// 收口——返回 (hits, true) 当且仅当查询成功且长度与样本一致；err / panic
// / 长度不符 → (nil, false)，静默跳过本轮（错误≠失效，不触发重建；不
// log：包内无 logger，对齐既有 recover 静默风格）。
func (c *prefillCoordinator) callExistsMulti(ctx context.Context, samples []any) (hits []bool, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			hits, ok = nil, false
		}
	}()
	h, err := c.inner.ExistsMulti(ctx, samples...)
	if err != nil || len(h) != len(samples) {
		return nil, false // 长度不符按本轮跳过（防御，契约上不应发生）
	}
	return h, true
}

// syncOnce 同步权威状态到本地：GET 成功（含键缺失→Uninitialized）则
// store 相位；失败（含 IsUnavailable）不更新（让 age 增长触发 T10）。
// 同步后权威相位为 Uninitialized/Failed → 本地 in-flight CAS 成功才
// 后台 force=0 重建；Building/Ready 不动作。
//
// G3 观测（不改任何既有行为）：GET 失败（非 NotFound）拍把底层错误原样
// 记入 syncErr 快照后照旧 return；本拍全 IO 成功（GET 成功，含 NotFound
// ——键缺失是正常相位非错误）则清 err 保留 at。记录点仅在本路径，
// probeOnce 不记（应用取样 bug 不得混入 Redis 故障信号）。
func (c *prefillCoordinator) syncOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), prefillStatusTimeout)
	defer cancel()
	v, err := c.rdb.Get(ctx, c.stateKey).Result()
	if err != nil {
		if !IsNotFound(err) {
			c.syncErr.Store(&errSnapshot{err: err, at: time.Now()})
			return // 失败不更新（T10：age 增长 → 热路径按非 Ready 降级）
		}
		c.storeLocal(PrefillUninitialized)
	} else {
		c.storeLocal(phaseFromValue(v))
	}
	// 成功清 nil 口径：执行到此处 = 本拍唯一一次 GET 已成功（失败分支已
	// return），"同拍内后发错误覆盖先发成功"天然满足；err 清 nil、at
	// 保留末次错误时间作诊断。Ready 相位的搭车校验（G1）若遇传输错误，
	// 会在 integrityCheck 内以 Store 覆盖本清零结果。
	c.clearSyncErr()
	if s := c.loadLocal(); s.phase == PrefillUninitialized || s.phase == PrefillFailed {
		c.triggerLazy(context.Background())
	}
	// G1 搭车完整性校验：时序红线——必须在既有「GET→storeLocal→尾部
	// triggerLazy」段之后（triggerLazy 的相位判定不得后移到校验降级
	// 之后，否则降级写的 Uninitialized 会让它同步起 force=0 → 被 acquire
	// 对 ready 拒绝 → 死锁面）。
	c.integrityCheck()
}

// integrityCheck 是 syncOnce 尾部的 G1 完整性搭车（ISSUE-102/106）：
// **仅当本拍权威相位==Ready** 时经 inner.IntegrityProbe 校验物理键
// 存在性/布局（building/fail/uninitialized 不校验——building 期键正在
// DEL/重建，校验必误报）：
//   - err≠nil（传输级，无结论）→ 记 syncErr（G3 口径：本拍非全成功），
//     不降级、不重建；
//   - invalid（ok=false, err=nil）→ storeLocal(Uninitialized) 本地即刻
//     降级（Exists 恢复恒 true，阻断假阴性透传）→ 异步 force=1 重建
//     （triggerIntegrityRebuild，禁止同步执行——会冻结 sync loop 整个
//     重建期，相位/LastSyncErr 观测断供）；
//   - ok → 本拍 GET+probe 全成功，clearSyncErr 已在 syncOnce 完成（G3
//     清 nil 口径的自然延伸）。
//
// 已知接受行为（ISSUE-107③）：force acquire 若传输失败，下拍 GET ready
// → 相位复活 → 再检 → 再试，每拍 ≤1 校验 RTT 透传窗，无害自愈；
// 「State 权威 + 本地相位」组合对不可逆决策仍有 ≤syncInterval 假窗口。
func (c *prefillCoordinator) integrityCheck() {
	if s := c.loadLocal(); s.phase != PrefillReady {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), prefillStatusTimeout)
	defer cancel()
	ok, err := c.inner.IntegrityProbe(ctx)
	if err != nil {
		c.syncErr.Store(&errSnapshot{err: err, at: time.Now()}) // 无结论：不降级不重建
		return
	}
	if !ok {
		c.storeLocal(PrefillUninitialized) // 本地即刻降级
		c.triggerIntegrityRebuild()
	}
}

// triggerIntegrityRebuild 起异步 force=1 完整性重建（复用 triggerLazy
// 骨架改 force=1：in-flight CAS 防重入；ctx 直接以 Background 派生
// RebuildTimeout 预算——syncOnce 无调用方 ctx 可继承，无剥离语义）。
// **必须异步**——syncOnce 内同步 run 会冻结 sync loop 整个
// 重建期。抢占失败（多实例他方持锁 Building）返回的 ErrRebuildInProgress
// 静默丢弃：仲裁由权威 acquire Lua 完成，本实例经下拍同步自愈。
func (c *prefillCoordinator) triggerIntegrityRebuild() {
	if !c.inFlight.CompareAndSwap(false, true) {
		return
	}
	runCtx, cancel := context.WithTimeout(context.Background(), c.cfg.rebuildTimeout)
	go func() {
		defer cancel()
		defer c.inFlight.Store(false) // 就地清 in-flight（持 CAS 所有权）
		_ = c.run(runCtx, 1)
	}()
}

// clearSyncErr 按 G3 口径清除末次错误：err 置 nil，at 保留（尚未记录过
// 错误时指针为 nil，无需写入）。
//
// syncErr 单写者契约：写方仅 syncOnce 路径（loop goroutine 内单写者
// 串行）；本方法的 Load-then-Store 依赖该前提，非原子。新增记录点
// （run/finish 等其它 goroutine）前，须先把写入收口到单写者或改为
// CAS，否则错误快照可能被并发写撕裂。
func (c *prefillCoordinator) clearSyncErr() {
	if prev := c.syncErr.Load(); prev != nil && prev.err != nil {
		c.syncErr.Store(&errSnapshot{at: prev.at})
	}
}

// triggerLazy 热路径/同步后的惰性触发：按本地相位短路后，in-flight CAS
// false→true 成功才起后台 force=0 重建（不排队不等锁；抢占失败由 run
// 静默处理）。ctx = WithoutCancel(parent) + WithTimeout(RebuildTimeout)。
//
// 相位短路（与 syncOnce 仅 Uninitialized/Failed 才触发的口径对齐，避免
// 高 QPS 降级期对 Redis 反向加压）：
//   - Building：持锁中，直接 return（不发 acquire）；
//   - Ready：一律不触发——新鲜时无需触发；stale（本地快照超龄，N4）时
//     同样 return，触发权交还下轮 syncOnce 权威刷新（≤1 个 syncInterval）：
//     权威仍 Ready 则刷新为新鲜恢复服务，权威已变 Uninitialized/Failed
//     则 syncOnce 同步后立即触发。避免拿不可信的本地视图对权威 Ready
//     发必然被拒的空转 acquire；
//   - Failed：本地冷却 nextTriggerAt 未到期不发；到期先续一个估算退避
//     窗（本地近似=retryInitial，权威仍由 fail TTL 把守）再放行本次；
//   - Uninitialized：不冷却。
func (c *prefillCoordinator) triggerLazy(parent context.Context) {
	s := c.loadLocal()
	switch s.phase {
	case PrefillBuilding:
		return
	case PrefillReady:
		return // 新鲜与 stale 一律不触发，stale 由 syncOnce 负责（N4）
	case PrefillFailed:
		if time.Now().UnixNano() < c.nextTriggerAt.Load() {
			return // 本地退避冷却中
		}
		// 冷却到期：续一个估算退避窗再放行（防到期后逐请求裸奔 acquire）
		c.nextTriggerAt.Store(time.Now().Add(c.cfg.retryInitial).UnixNano())
	case PrefillUninitialized:
		// 不冷却
	}
	if !c.inFlight.CompareAndSwap(false, true) {
		return
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), c.cfg.rebuildTimeout)
	go func() {
		defer cancel()
		defer c.inFlight.Store(false) // 就地清 in-flight（持 CAS 所有权）
		_ = c.run(runCtx, 0)
	}()
}

// run 执行一次完整重建流程：acquire 抢占 → Δ 等待（ctx 可取消）→
// inner.Reset（清空+connectAll）→ fn（panic recover + RebuildTimeout 预算）
// → 成功 prefillReady / 失败 prefillFail → 就地更新本地相位。
//
// force=1（显式 Reset）：抢占失败返回 ErrRebuildInProgress；
// force=0（自动触发）：抢占失败静默返回 nil。in-flight 由触发方持有，
// 本函数不触碰（显式路径互斥由权威 acquire 仲裁）。
func (c *prefillCoordinator) run(ctx context.Context, force int) error {
	// R5：与协调器生命周期 ctx 合并——Close 时取消在飞 run（finish 走
	// Canceled 禁写分支）；调用方 ctx 的超时/取消照常传导。
	runCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.baseCtx, cancel)
	defer func() {
		stop()
		cancel()
	}()

	ttl := prefillBuildingTTL(c.cfg.rebuildTimeout, c.cfg.syncInterval)
	ok, err := c.acquire(runCtx, force, ttl)
	if err != nil {
		return err // 状态键读写失败：不持锁不记账，原样返回
	}
	if !ok {
		if force == 1 {
			return ErrRebuildInProgress // T5：Building 中显式请求立即拒绝
		}
		return nil // T3/T2：自动触发抢占失败静默不动
	}

	// Δ 传播等待：让并发实例的本地缓存先看到 building（ctx 可取消；
	// 取消即中止且不写状态——building TTL 过期自愈，防迟到写）。
	if delta := 2 * c.cfg.syncInterval; delta > 0 {
		tm := time.NewTimer(delta)
		select {
		case <-runCtx.Done():
			tm.Stop()
			return runCtx.Err()
		case <-tm.C:
		}
	}

	if err := c.inner.Reset(runCtx); err != nil {
		return c.finish(runCtx, err)
	}
	return c.finish(runCtx, c.callFn(runCtx))
}

// acquire 执行抢占 Lua，返回是否持锁成功（acquireTries 计数供测试判别）。
func (c *prefillCoordinator) acquire(ctx context.Context, force int, ttl time.Duration) (bool, error) {
	c.acquireTries.Add(1)
	n, err := prefillAcquireScript.Run(ctx, c.rdb,
		[]string{c.stateKey, c.failnKey}, ttl.Milliseconds(), force).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// callFn 执行应用回灌：独立 RebuildTimeout 预算 + panic recover 转 error
// （走 fail 路径，不崩溃进程）。
func (c *prefillCoordinator) callFn(ctx context.Context) (err error) {
	fnCtx, cancel := context.WithTimeout(ctx, c.cfg.rebuildTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("redis: bloom prefill fn panic: %v", r)
		}
	}()
	return c.fn(fnCtx, &prefillIngest{inner: c.inner})
}

// finish 收尾状态记账（T6/T7 与防迟到写的调和）：
//   - 成功：run ctx 已 Done 不写 ready（防迟到写，building TTL 自愈）；
//     否则 prefillReady，脚本返回 1 才就地 store Ready（返回 0 属状态
//     漂移，忽略并交下轮同步纠正）。
//   - 失败：run ctx 为调用方主动取消（Canceled）不写任何状态（防迟到写）；
//     其余失败（fn error/panic/预算超时）以 WithoutCancel 派生的独立短
//     超时 ctx 写 prefillFail——保证 T7"ctx 超时→fail"可落地；预算超时
//     包装为 ErrRebuildTimeout；写成功才 store Failed。
func (c *prefillCoordinator) finish(runCtx context.Context, cause error) error {
	if cause == nil {
		if runCtx.Err() != nil {
			return nil // 防迟到写：成功结果但 ctx 已死，不写 ready
		}
		ok, err := c.prefillReady(runCtx)
		if err != nil {
			return err
		}
		if ok {
			c.storeLocal(PrefillReady)
		}
		return nil
	}

	if errors.Is(runCtx.Err(), context.Canceled) {
		return cause // 调用方取消：不写状态（防迟到写），TTL 自愈
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		cause = fmt.Errorf("%w: %w", ErrRebuildTimeout, cause)
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), prefillStatusTimeout)
	defer cancel()
	if err := c.prefillFail(wctx); err == nil {
		c.storeLocal(PrefillFailed)
	}
	return cause
}

// prefillReady 执行 T6 条件转移：仍=building 才置 ready 并归零 failn，
// 返回是否发生转移。
func (c *prefillCoordinator) prefillReady(ctx context.Context) (bool, error) {
	n, err := prefillReadyScript.Run(ctx, c.rdb, []string{c.stateKey, c.failnKey}).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// prefillFail 执行 T7 无条件记账。backoff 依赖 INCR 后的 n，而 Lua 不做
// 算术、毫秒须客户端算好传入——故先 GET failn 预读预测 n=当前+1 算好
// backoff 传入，脚本内 INCR 得权威 n（单实例串行下预测精确；并发失败时
// 退避值可能偏小一档，下轮自愈）。
func (c *prefillCoordinator) prefillFail(ctx context.Context) error {
	n0 := int64(0)
	if v, err := c.rdb.Get(ctx, c.failnKey).Int64(); err == nil {
		n0 = v
	} // 含键缺失（redis.Nil）与瞬态错误 → 按 0 预测，脚本 INCR 仍权威
	bo := backoffDuration(n0+1, c.cfg.retryInitial, c.cfg.retryMax)
	// R2：按真实 backoff 写入本地 Failed 冷却（热路径在窗内不再发 acquire）
	c.nextTriggerAt.Store(time.Now().Add(bo).UnixNano())
	_, err := prefillFailScript.Run(ctx, c.rdb,
		[]string{c.stateKey, c.failnKey}, bo.Milliseconds()).Int64()
	return err
}

// readState 是 State 的 1 RTT 权威查询：直接 GET 状态键，不经本地缓存。
func (c *prefillCoordinator) readState(ctx context.Context) (PrefillPhase, error) {
	v, err := c.rdb.Get(ctx, c.stateKey).Result()
	if err != nil {
		if IsNotFound(err) {
			return PrefillUninitialized, nil
		}
		return PrefillUninitialized, err
	}
	return phaseFromValue(v), nil
}
