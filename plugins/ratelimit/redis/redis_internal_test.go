package redis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charlienet/gadget/ratelimit"
)

func TestNewNilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(nil) 必须 panic（fail-fast）")
		}
	}()
	New(nil)
}

func TestModeConstantsAlignGrantMode(t *testing.T) {
	// ARGV[5] 约定：0=BestEffort / 1=AllOrNothing，必须与 core 枚举序号一致。
	if modeBestEffort != int(ratelimit.GrantBestEffort) {
		t.Fatalf("modeBestEffort=%d，GrantBestEffort=%d", modeBestEffort, ratelimit.GrantBestEffort)
	}
	if modeAllOrNothing != int(ratelimit.GrantAllOrNothing) {
		t.Fatalf("modeAllOrNothing=%d，GrantAllOrNothing=%d", modeAllOrNothing, ratelimit.GrantAllOrNothing)
	}
}

func TestParseWholesaleResult(t *testing.T) {
	cases := []struct {
		name        string
		in          any
		wantGranted int
		wantRetry   time.Duration
		wantErr     bool
	}{
		{"足额授予 retry 占位 -1", []any{int64(3), int64(7), "-1", "12.5"}, 3, 0, false},
		{"部分授予", []any{int64(2), int64(0), "-1", "0.8"}, 2, 0, false},
		{"拒绝带 retry", []any{int64(0), int64(0), "0.5", "60"}, 0, 500 * time.Millisecond, false},
		{"拒绝小数秒全精度", []any{int64(0), int64(2), "1.25", "3"}, 0, 1250 * time.Millisecond, false},
		{"granted 负值归零", []any{int64(-1), int64(0), "-1", "0"}, 0, 0, false},
		{"非数组", "junk", 0, 0, true},
		{"长度不足", []any{int64(1), int64(0), "-1"}, 0, 0, true},
		{"granted 非整数", []any{"x", int64(0), "-1", "0"}, 0, 0, true},
		{"retry 非字符串", []any{int64(0), int64(0), 1.5, "0"}, 0, 0, true},
		{"retry 非数字串", []any{int64(0), int64(0), "abc", "0"}, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, r, err := parseWholesaleResult(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应报错，got %v/%v", g, r)
				}
				// 协议错误不得伪装成"后端不可用"（不触发兜底）。
				if errors.Is(err, ratelimit.ErrBackendUnavailable) {
					t.Fatalf("结构异常错误不得含 ErrBackendUnavailable，got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if g != tc.wantGranted || r != tc.wantRetry {
				t.Fatalf("got %d/%v，期望 %d/%v", g, r, tc.wantGranted, tc.wantRetry)
			}
		})
	}
}

// timeoutNetErr 实现 net.Error 且 Timeout()==true。
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return false }

func TestIsUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"连接拒绝 OpError", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"io.EOF", io.EOF, true},
		{"意外 EOF", io.ErrUnexpectedEOF, true},
		{"net 超时", timeoutNetErr{}, true},
		{"连接池超时", errors.New("redis: connection pool timeout"), true},
		{"client 已关闭", errors.New("redis: client is closed"), true},
		{"调用方取消", context.Canceled, false},
		{"调用方超时", context.DeadlineExceeded, false},
		{"命令级错误", errors.New("WRONGTYPE Operation against a key holding the wrong kind of value"), false},
		{"Lua 运行错误", errors.New("ERR Error in script"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnavailable(tc.err); got != tc.want {
				t.Fatalf("isUnavailable(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestWithKeyPrefix(t *testing.T) {
	// goredis client 构造是惰性的：不发命令则不建连接，无需真实 Redis。
	shared := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379"})
	defer shared.Close()

	b := New(shared).(*Backend)
	if b.prefix != defaultKeyPrefix || b.limitKey("k") != "rate:k" {
		t.Fatalf("默认前缀应为 rate:，got %q", b.prefix)
	}

	b2 := New(shared, WithKeyPrefix("ns:")).(*Backend)
	if b2.limitKey("k") != "ns:k" {
		t.Fatalf("WithKeyPrefix 未生效，got %q", b2.prefix)
	}
	b3 := New(shared, WithKeyPrefix("")).(*Backend)
	if b3.prefix != defaultKeyPrefix {
		t.Fatalf("空前缀应防御式忽略保默认，got %q", b3.prefix)
	}
}

// newUnreachableClient 指向必然被拒的端口（连接层故障，无需真实 Redis）。
func newUnreachableClient() *goredis.Client {
	return goredis.NewClient(&goredis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 300 * time.Millisecond,
		MaxRetries:  -1,
	})
}

func testSpec() ratelimit.Spec {
	return ratelimit.Spec{Rate: 100, Per: time.Second, Burst: 10, IdleRetention: time.Minute}
}

func TestWholesaleUnavailableWrapping(t *testing.T) {
	client := newUnreachableClient()
	defer client.Close()
	b := New(client)

	_, _, err := b.Wholesale(context.Background(), "k", 1, testSpec(), ratelimit.GrantBestEffort)
	if err == nil {
		t.Fatal("不可达后端必须报错")
	}
	if !errors.Is(err, ratelimit.ErrBackendUnavailable) {
		t.Fatalf("连接类故障应包装 ErrBackendUnavailable，got %v", err)
	}
	if errors.Is(err, ratelimit.ErrFailOpen) {
		t.Fatalf("后端不得自行兜底（ErrFailOpen 是 core 的职责），got %v", err)
	}
}

func TestWholesaleCtxPassthrough(t *testing.T) {
	client := newUnreachableClient()
	defer client.Close()
	b := New(client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := b.Wholesale(ctx, "k", 1, testSpec(), ratelimit.GrantBestEffort)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 取消必须原样透传 ctx.Err()，got %v", err)
	}
	if errors.Is(err, ratelimit.ErrBackendUnavailable) {
		t.Fatalf("ctx 错误不得包装为不可用（契约三条之三条），got %v", err)
	}

	// 执行中超时：同样透传 DeadlineExceeded 而非不可用。
	tctx, tcancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer tcancel()
	time.Sleep(2 * time.Millisecond) // 确保 ctx 已过期
	_, _, err = b.Wholesale(tctx, "k", 1, testSpec(), ratelimit.GrantBestEffort)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx 超时应透传 DeadlineExceeded，got %v", err)
	}
}

func TestSentinelNotExportedTwice(t *testing.T) {
	// 插件绝不重新定义哨兵语义，只引用 core 的：包装串必须走 ratelimit 前缀。
	client := newUnreachableClient()
	defer client.Close()
	_, _, err := New(client).Wholesale(context.Background(), "k", 1, testSpec(), ratelimit.GrantBestEffort)
	if err != nil && !strings.Contains(err.Error(), ratelimit.ErrBackendUnavailable.Error()) {
		t.Fatalf("包装错误应挂 core 哨兵原文，got %v", err)
	}
}

// --- BestEffort 对照测试（真实 wholesaleScript，floor 回归可被拦截）---

// 与外部集成测试同源的 REDIS_URL 守卫（N4：不 import gadget/redis 的 test 包）。

var rlKeyCounter uint64

// newRealRedis 基于 REDIS_URL 建连：未设置则 skip；不可达则 fail（防假绿）。
func newRealRedis(t *testing.T) *goredis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skipf("REDIS_URL 未设置，跳过真实 Redis 测试（脚本语义未验证）")
	}
	opt, err := goredis.ParseURL(url)
	require.NoErrorf(t, err, "解析 REDIS_URL 失败: %s", url)
	rdb := goredis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoErrorf(t, rdb.Ping(ctx).Err(), "无法连接真实 Redis（%s），测试失败以避免假绿", url)
	return rdb
}

// scriptKey 生成测试作用域唯一键并注册删除收尾（{} hash tag 兼容 Cluster）。
func scriptKey(t *testing.T, rdb *goredis.Client, base string) string {
	t.Helper()
	id := atomic.AddUint64(&rlKeyCounter, 1)
	key := fmt.Sprintf("{gadget-rlredis-test}:%s:%d:%d", base, time.Now().UnixNano(), id)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = rdb.Del(ctx, key).Err()
	})
	return key
}

// baselineAtMostScript 是 gadget/redis 模块 tokenBucketAtMostScript
// （redis/ratelimit.go:71-114）的逐字复制，**仅作对照测试基线**；原脚本
// 属冻结公共 API，此处不改动其内容。
var baselineAtMostScript = goredis.NewScript(`
-- this script has side-effects, so it requires replicate commands mode
redis.replicate_commands()

local rate_limit_key = KEYS[1]
local burst = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local period = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])

local emission_interval = period / rate
local burst_offset = emission_interval * burst

local jan_1_2017 = 1483228800
local now = redis.call('TIME')
now = (now[1] - jan_1_2017) + (now[2] / 1000000)

local tat = tonumber(redis.call('GET', rate_limit_key) or '0')
tat = math.max(tat, now)

local diff = now - (tat - burst_offset)
local remaining = diff / emission_interval

if remaining < 1 then
	local reset_after = tat - now
	local retry_after = emission_interval - diff
	return {0, 0, tostring(retry_after), tostring(reset_after)}
end

if remaining < cost then
	cost = remaining
	remaining = 0
else
	remaining = remaining - cost
end

local new_tat = tat + emission_interval * cost

local reset_after = new_tat - now
if reset_after > 0 then
	redis.call('SET', rate_limit_key, new_tat, 'EX', math.ceil(reset_after))
end
return {cost, remaining, tostring(-1), tostring(reset_after)}
`)

// scriptResult 是脚本返回值的测试侧解析结构。
type scriptResult struct {
	granted    int64
	remaining  int64
	retryAfter float64
	resetAfter float64
}

func runScript(t *testing.T, s *goredis.Script, rdb *goredis.Client, key string, args ...any) scriptResult {
	t.Helper()
	v, err := s.Run(context.Background(), rdb, []string{key}, args...).Result()
	require.NoError(t, err)
	values, ok := v.([]any)
	require.Truef(t, ok && len(values) >= 4, "脚本返回异常结构: %v", v)
	granted, ok := values[0].(int64)
	require.Truef(t, ok, "granted 非整数: %T", values[0])
	remaining, ok := values[1].(int64)
	require.Truef(t, ok, "remaining 非整数: %T", values[1])
	retryStr, ok := values[2].(string)
	require.Truef(t, ok, "retry_after 非字符串: %T", values[2])
	resetStr, ok := values[3].(string)
	require.Truef(t, ok, "reset_after 非字符串: %T", values[3])
	retry, err := strconv.ParseFloat(retryStr, 64)
	require.NoError(t, err)
	reset, err := strconv.ParseFloat(resetStr, 64)
	require.NoError(t, err)
	return scriptResult{granted: granted, remaining: remaining, retryAfter: retry, resetAfter: reset}
}

// slicesOf 复制 args 并追加元素（避免 append 复用底层数组）。
func slicesOf(args []any, extra ...any) []any {
	out := make([]any, 0, len(args)+len(extra))
	out = append(out, args...)
	return append(out, extra...)
}

// serverTat 计算"以服务端当前时刻为基准、剩余 r0 个令牌"时 GCRA 应存储的
// TAT 值（脚本内 now 以 jan_1_2017 为基准偏移，与脚本常量同源）。
func serverTat(t *testing.T, rdb *goredis.Client, burst, rate int, per time.Duration, r0 float64) string {
	t.Helper()
	const jan1_2017 = 1483228800
	serverNow, err := rdb.Time(context.Background()).Result()
	require.NoError(t, err)
	nowOffset := float64(serverNow.Unix()-jan1_2017) + float64(serverNow.Nanosecond())/1e9
	interval := per.Seconds() / float64(rate)
	tat := nowOffset + (float64(burst)-r0)*interval
	return strconv.FormatFloat(tat, 'f', 6, 64)
}

func getTat(t *testing.T, rdb *goredis.Client, key string) float64 {
	t.Helper()
	s, err := rdb.Get(context.Background(), key).Result()
	require.NoError(t, err)
	v, err := strconv.ParseFloat(s, 64)
	require.NoError(t, err)
	return v
}

// TestBestEffortAgainstOriginalScriptBaseline 对照测试：同参数序列在两个
// 独立 key 上重放原脚本（基线）与**真实 wholesaleScript**（BestEffort），
// 逐值比对。走包内未导出脚本变量（而非测试副本），script.go 的 floor
// 一旦回退，本测试必须变红。
//
// 唯一预期差异（N5）：部分授予时基线以**小数 cost** 推进 TAT（返回时经
// RESP integer 向下截断，蒸发 ≤1 令牌的小数部分），改造版 floor 后推进
// （扣减量==返回量）。据此分三类断言：
//
//	甲（整数足额轮）：四字段逐值一致（整数授予无小数蒸发，两版语义等价）；
//	乙（部分裁剪轮）：granted/remaining/retry_after 逐值一致（返回都被
//	  int 截断），差异体现在 TAT 推进与 reset_after 上——差值 == 被蒸发的
//	  小数部分 × emission_interval；
//	丙（蒸发累积轮）：裁剪轮之后隔 2.5s 请求 1 个令牌——基线剩余被蒸发
//	  至 <1 而拒绝，改造版保留的小数回补后 ≥1 可授予。这是全序列唯一一处
//	  返回值差异（granted 0 vs 1），方向恒为"改造版不亏"。
func TestBestEffortAgainstOriginalScriptBaseline(t *testing.T) {
	rdb := newRealRedis(t)
	ctx := context.Background()

	const (
		rate  = 3
		burst = 20
	)
	per := 10 * time.Second
	interval := per.Seconds() / rate // 3.3333s
	args := []any{burst, rate, per.Seconds()}

	// 预置 remaining = 10.5（TAT 带 0.5 小数余额）的独立 key 对。
	newPair := func(name string, r0 float64) (string, string) {
		kA := scriptKey(t, rdb, name+"-base")
		kB := scriptKey(t, rdb, name+"-mod")
		tat := serverTat(t, rdb, burst, rate, per, r0)
		require.NoError(t, rdb.Set(ctx, kA, tat, 0).Err())
		require.NoError(t, rdb.Set(ctx, kB, tat, 0).Err())
		return kA, kB
	}
	mod := func(key string, cost int) scriptResult {
		return runScript(t, wholesaleScript, rdb, key, slicesOf(args, cost, modeBestEffort)...)
	}

	// --- 甲：整数足额授予轮，必须逐值一致 ---
	kA1, kB1 := newPair("eq", 10.5)
	a1 := runScript(t, baselineAtMostScript, rdb, kA1, slicesOf(args, 10)...)
	b1 := mod(kB1, 10)
	assert.Equal(t, int64(10), a1.granted)
	assert.Equal(t, a1.granted, b1.granted, "足额轮 granted 必须一致")
	assert.Equal(t, a1.remaining, b1.remaining, "足额轮 remaining 必须一致")
	assert.Equal(t, int64(0), a1.remaining)
	assert.Equal(t, a1.retryAfter, b1.retryAfter)
	assert.InDelta(t, a1.resetAfter, b1.resetAfter, 0.05, "足额轮无小数蒸发，reset_after 亦一致（容差=EVAL 间隔漂移）")

	// --- 乙：部分裁剪轮，返回一致、TAT 推进暴露 N5 差异 ---
	kA2, kB2 := newPair("clip", 10.5)
	a2 := runScript(t, baselineAtMostScript, rdb, kA2, slicesOf(args, 11)...)
	b2 := mod(kB2, 11)
	assert.Equal(t, int64(10), a2.granted, "基线 10.5 经 RESP 截断返回 10")
	assert.Equal(t, a2.granted, b2.granted, "裁剪轮返回的 granted 一致（floor 与 int 截断同值）")
	assert.Equal(t, a2.remaining, b2.remaining)
	assert.Equal(t, a2.retryAfter, b2.retryAfter)
	// N5 核心断言：基线多推进的小数部分（0.5 令牌）即蒸发量，
	// 体现为 reset_after 与 TAT 存量差 ≈ 0.5 × emission_interval。
	assert.InDelta(t, 0.5*interval, a2.resetAfter-b2.resetAfter, 0.1, "reset_after 差即被蒸发的 0.5 令牌")
	tatA2, tatB2 := getTat(t, rdb, kA2), getTat(t, rdb, kB2)
	assert.InDelta(t, 0.5*interval, tatA2-tatB2, 0.1, "TAT 差即被蒸发的 0.5 令牌（改造版扣减量==返回量）")

	// --- 丙：蒸发累积轮（唯一预期返回值差异）---
	// 裁剪轮后：基线剩余 ≈0.0、改造版剩余 ≈0.5。静置 2.5s（回补 0.75，
	// 处于窗口 [1.67s, 3.33s)：基线 <1 拒绝、改造版 ≥1 可授 1 枚）。
	time.Sleep(2500 * time.Millisecond)
	a3 := runScript(t, baselineAtMostScript, rdb, kA2, slicesOf(args, 1)...)
	b3 := mod(kB2, 1)
	assert.Equal(t, int64(0), a3.granted, "基线剩余被蒸发殆尽 → 拒绝")
	assert.Equal(t, int64(1), b3.granted, "改造版保留 0.5 余额回补后可授予 → 唯一预期差异")
	assert.Greater(t, a3.retryAfter, 0.0, "拒绝方必须给出正 retry_after")
	assert.Equal(t, -1.0, b3.retryAfter, "授予方 retry_after 为 -1 占位")
}
