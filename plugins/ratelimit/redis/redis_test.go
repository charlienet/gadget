package redis_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redislimit "github.com/charlienet/gadget/plugins/ratelimit/redis"
	"github.com/charlienet/gadget/ratelimit"
)

// keyCounter 保证同一进程内多次运行/并发子测试生成互不相同的键。
var keyCounter uint64

// rateKey 生成一次测试作用域专用的唯一 Redis 键并注册删除收尾。
// 键带 {} hash tag，兼容 Redis Cluster。
func rateKey(t *testing.T, rdb goredis.Cmdable, base string) string {
	t.Helper()
	id := atomic.AddUint64(&keyCounter, 1)
	key := fmt.Sprintf("{gadget-rlredis-test}:%s:%d:%d", base, time.Now().UnixNano(), id)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = rdb.Del(ctx, key)
	})
	return key
}

// TestMain 在 REDIS_URL 未设置时向 stderr 打印显眼警告，
// 避免真实 Redis 验证被静默跳过。
func TestMain(m *testing.M) {
	if os.Getenv("REDIS_URL") == "" {
		fmt.Fprintln(os.Stderr, "=======================================================================")
		fmt.Fprintln(os.Stderr, "[WARNING] 环境变量 REDIS_URL 未设置：")
		fmt.Fprintln(os.Stderr, "          GCRA 脚本语义（BestEffort 对照/AllOrNothing/错误透传）")
		fmt.Fprintln(os.Stderr, "          未经真实 Redis 验证，以下集成测试将全部 SKIP。")
		fmt.Fprintln(os.Stderr, "=======================================================================")
	}
	os.Exit(m.Run())
}

// newTestClient 基于真实 Redis（REDIS_URL）创建 go-redis Client。
// REDIS_URL 未设置时跳过；已设置但不可达时直接失败，避免连接问题伪装成通过。
func newTestClient(t *testing.T) goredis.Cmdable {
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

// TestAllOrNothingSemantics 验证精确扣减分支（H3 防蒸发）。
func TestAllOrNothingSemantics(t *testing.T) {
	rdb := newTestClient(t)
	backend := redislimit.New(goredis.NewClient(mustParseURL(t))).(*redislimit.Backend)
	t.Cleanup(func() { _ = backend.Close() })

	ctx := context.Background()
	spec := ratelimit.Spec{Rate: 100, Per: time.Second, Burst: 5, IdleRetention: time.Minute}

	t.Run("冷启动满桶无死角", func(t *testing.T) {
		key := rateKey(t, rdb, "aon-cold")
		g, retry, err := backend.Wholesale(ctx, key, 5, spec, ratelimit.GrantAllOrNothing)
		require.NoError(t, err)
		assert.Equal(t, 5, g, "冷启动 tat=now → remaining=Burst，满额请求应放行")
		assert.Zero(t, retry)
	})

	t.Run("拒绝不扣减且无SET副作用", func(t *testing.T) {
		key := rateKey(t, rdb, "aon-nodebit")
		// 先满额放行 5，桶耗尽；此后 key 已存在（TAT+EX）。
		g, _, err := backend.Wholesale(ctx, key, 5, spec, ratelimit.GrantAllOrNothing)
		require.NoError(t, err)
		require.Equal(t, 5, g)

		tat1, ttl1 := getTatTTL(t, rdb, backendKey(key))
		g2, retry2, err := backend.Wholesale(ctx, key, 1, spec, ratelimit.GrantAllOrNothing)
		require.NoError(t, err)
		assert.Zero(t, g2, "remaining<1 必须拒绝")
		assert.Greater(t, retry2, time.Duration(0), "拒绝应携带 retryAfter")
		tat2, ttl2 := getTatTTL(t, rdb, backendKey(key))
		assert.Equal(t, tat1, tat2, "拒绝后 TAT 必须原样保留（不推进、不 SET）")
		assert.Greater(t, ttl2, time.Duration(0))
		assert.InDelta(t, ttl1.Seconds(), ttl2.Seconds(), 1, "拒绝不得重设 TTL")
	})

	t.Run("首次不足额拒绝不写入", func(t *testing.T) {
		key := rateKey(t, rdb, "aon-coldshort")
		// want=6 > Burst=5：冷启动 remaining=5 < 6 → 拒绝且零副作用。
		g, retry, err := backend.Wholesale(ctx, key, 6, spec, ratelimit.GrantAllOrNothing)
		require.NoError(t, err)
		assert.Zero(t, g)
		assert.Greater(t, retry, time.Duration(0))
		_, err = rdb.Get(ctx, backendKey(key)).Result()
		assert.ErrorIs(t, err, goredis.Nil, "拒绝不得产生任何 SET（key 不应存在）")
	})

	t.Run("拒绝后回满再放行", func(t *testing.T) {
		key := rateKey(t, rdb, "aon-refill")
		_, _, _ = backend.Wholesale(ctx, key, 5, spec, ratelimit.GrantAllOrNothing) // 耗尽
		g, _, err := backend.Wholesale(ctx, key, 3, spec, ratelimit.GrantAllOrNothing)
		require.NoError(t, err)
		assert.Zero(t, g, "耗尽后立即请求 3 应被拒")
		time.Sleep(60 * time.Millisecond) // 100/s → 远超 3 个令牌的回补窗口
		g2, _, err := backend.Wholesale(ctx, key, 3, spec, ratelimit.GrantAllOrNothing)
		require.NoError(t, err)
		assert.Equal(t, 3, g2, "回补后应恢复放行（EX 过期/TAT 追平 → 重新计满）")
	})
}

// backendKey 复现插件默认前缀拼接。
func backendKey(key string) string { return "rate:" + key }

func mustParseURL(t *testing.T) *goredis.Options {
	t.Helper()
	opt, err := goredis.ParseURL(os.Getenv("REDIS_URL"))
	require.NoError(t, err)
	return opt
}

func getTatTTL(t *testing.T, rdb goredis.Cmdable, key string) (float64, time.Duration) {
	t.Helper()
	ctx := context.Background()
	v, err := rdb.Get(ctx, key).Result()
	require.NoError(t, err)
	tat, err := strconv.ParseFloat(v, 64)
	require.NoError(t, err)
	ttl, err := rdb.TTL(ctx, key).Result()
	require.NoError(t, err)
	return tat, ttl
}

// TestCommandErrorPassThrough 命令级错误（Lua 运行错误）必须原样透传，
// 不得包装 ErrBackendUnavailable（防配置/数据错误被 FailOpen 掩盖）。
func TestCommandErrorPassThrough(t *testing.T) {
	rdb := newTestClient(t)
	backend := redislimit.New(goredis.NewClient(mustParseURL(t))).(*redislimit.Backend)
	t.Cleanup(func() { _ = backend.Close() })

	key := rateKey(t, rdb, "cmderr")
	require.NoError(t, rdb.Set(context.Background(), backendKey(key), "not-a-number", time.Minute).Err())

	_, _, err := backend.Wholesale(context.Background(), key, 1,
		ratelimit.Spec{Rate: 10, Per: time.Second, Burst: 10}, ratelimit.GrantBestEffort)
	require.Error(t, err, "TAT 值损坏应触发 Lua 运行错误")
	assert.NotErrorIs(t, err, ratelimit.ErrBackendUnavailable, "命令级错误不得包装为不可用")
	assert.NotErrorIs(t, err, ratelimit.ErrExceeded)
}

// TestWithCoreLimiter 与 core 的端到端集成：Allow 租约批发、Wait、
// Close 幂等。
func TestWithCoreLimiter(t *testing.T) {
	newTestClient(t) // 守卫 REDIS_URL（未设置则 skip；不可达则 fail）
	rdb := goredis.NewClient(mustParseURL(t))

	ns := fmt.Sprintf("{gadget-rlredis-test}:core:%d:", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		keys, err := rdb.Keys(ctx, ns+"*").Result()
		if err == nil && len(keys) > 0 {
			_ = rdb.Del(ctx, keys...).Err()
		}
	})

	limiter := ratelimit.New(redislimit.New(rdb, redislimit.WithKeyPrefix(ns)),
		ratelimit.WithRate(100, time.Second),
		ratelimit.WithBurst(100),
		ratelimit.WithBackendTimeout(3*time.Second),
	)

	ctx := context.Background()

	// 冷启动桶满 100：want=clamp(round(100×1s/1s)×0.5,1,100)=50，
	// 反复批发直至拒绝——总放行量应等于桶理论量 100（±1，GCRA 浮点截断）。
	passed := 0
	for {
		ok, err := limiter.Allow(ctx, "user:1", 1)
		if !ok {
			require.ErrorIs(t, err, ratelimit.ErrExceeded)
			break
		}
		require.NoError(t, err)
		passed++
		require.Less(t, passed, 200, "放行量失控：疑似双重发币")
	}
	assert.InDelta(t, 100, passed, 1, "经 Redis 后端的总放行量应等于桶理论量")

	// 等待回补：速率 100/s，几毫秒即可再放行。
	require.NoError(t, limiter.Wait(ctx, "user:2", 2))

	// Close 幂等（core Once + Backend io.Closer 恰好一次释放）。
	require.NoError(t, limiter.Close())
	require.NoError(t, limiter.Close())
	_, err := limiter.Allow(ctx, "user:1", 1)
	require.ErrorIs(t, err, ratelimit.ErrClosed)
}
