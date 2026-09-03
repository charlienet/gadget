package redis_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	urlpkg "net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/redis/test"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 回归提醒（P2-8）：以下场景无法在本地/CI 环境模拟，需在 go-redis 升级时
// 以真实 Redis 验证：
//  1. 集群重定向（MOVED/ASK）：前缀改写须在重定向后仍正确，验证项为
//     AddPrefix/前缀 hook 在 ClusterClient 上的行为。
//  2. 命令重试（MaxRetries>0 且命令失败时自动重发）：重试不应导致前缀
//     二次添加（hook 只改写原始参数，重试复用同一 Cmder，理论安全）。
//
// TestPipelinePrefix 覆盖了 pipeline 场景下前缀只加一次的回归（miniredis 可测）。

// firstAddrURL 将逗号分隔多地址的 redis:// URL 截取为第一个地址的 URL；
// 单地址 URL 原样返回。用于单机 goredis.ParseURL 无法解析逗号分隔 host 的场景。
func firstAddrURL(rawURL string) (string, error) {
	u, err := urlpkg.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if idx := strings.Index(u.Host, ","); idx != -1 {
		u.Host = u.Host[:idx]
	}
	return u.String(), nil
}

func randomHex(n int) string {
	bytes := make([]byte, n/2+1) // Generate enough bytes
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	hexStr := hex.EncodeToString(bytes)
	return hexStr[:n] // Return only n characters
}

// TestNewWithClient 验证 NewWithClient 的包装语义：
// 前缀 hook 生效、无前缀时纯包装、GracefulClose 不关闭外部传入的连接池。
// 注：一个 uc 只能包装一次，故带前缀与无前缀分别使用独立的 uc。
func TestNewWithClient(t *testing.T) {
	// 带前缀包装：写入 "k" 实际落到 "app:k"
	mr1, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr1.Close()

	uc1 := goredis.NewClient(&goredis.Options{Addr: mr1.Addr()})
	defer func() { _ = uc1.Close() }()

	rdb, err := redis.NewWithClient(uc1, redis.WithPrefix("app"))
	assert.NoError(t, err)

	assert.NoError(t, rdb.Set(context.Background(), "k", "v", time.Hour).Err())
	val, err := mr1.Get("app:k")
	assert.NoError(t, err)
	assert.Equal(t, "v", val)

	// GracefulClose 只级联关闭派生池，不关闭外部 uc：关闭后 uc 仍可用
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assert.NoError(t, rdb.GracefulClose(ctx))
	assert.NoError(t, uc1.Ping(context.Background()).Err())

	// 无前缀包装：直通，不加前缀
	mr2, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr2.Close()

	uc2 := goredis.NewClient(&goredis.Options{Addr: mr2.Addr()})
	defer func() { _ = uc2.Close() }()

	plain, err := redis.NewWithClient(uc2)
	assert.NoError(t, err)
	assert.NoError(t, plain.Set(context.Background(), "pk", "pv", time.Hour).Err())
	val, err = mr2.Get("pk")
	assert.NoError(t, err)
	assert.Equal(t, "pv", val)
}

func TestNewRedis(t *testing.T) {
	rdb := redis.New(
		redis.WithAddrs([]string{"192.168.2.222:6379"}),
		redis.WithPassword("123456"))

	assert.Nil(t, rdb.Constraint(redis.Ping()))
}

func TestRunMiniRedis(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		_ = rdb.Constraint(redis.Ping())
	})
}

func TestVersion(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		assert.NotNil(t, rdb.Constraint(redis.Version(">=10.0")))
	})
}

func TestPrefix(t *testing.T) {
	test.RunOnRedis(t, func(rdb redis.Client) {
		r1 := rdb.AddPrefix("h2")
		r1.Set(context.Background(), "abc", "abc", time.Hour)
	})
}

// TestAddPrefixCascadeClose 验证 AddPrefix 派生的子连接池会随父连接池
// 级联关闭（修复连接池泄漏），且 GracefulClose 幂等。
func TestAddPrefixCascadeClose(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		// 派生多个子连接池
		child1 := rdb.AddPrefix("h1")
		child2 := child1.AddPrefix("h2")

		assert.NoError(t, child1.Set(context.Background(), "k1", "v1", time.Hour).Err())
		assert.NoError(t, child2.Set(context.Background(), "k2", "v2", time.Hour).Err())

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// 关闭父连接池，应级联关闭所有派生连接池
		assert.NoError(t, rdb.GracefulClose(ctx))
		// 幂等：重复关闭不报错
		assert.NoError(t, rdb.GracefulClose(ctx))
	})
}

// TestParseUrl 验证 ParseURL 的 URL 解析：
// 覆盖单地址（无/带密码、带用户名、db path）、逗号分隔多地址（集群种子
// 列表）、哨兵（master_name 参数）、非法 URL 与 Option 覆盖，
// 断言解析结果的字段值而非仅 err。纯解析测试，不连接服务器。
// TestParseUrlNoPasswordLeak 验证 ParseURL 的错误消息不泄露 userinfo 中的密码
// （空 host 列表等解析错误场景，u.String() 会包含明文密码）。
func TestParseUrlNoPasswordLeak(t *testing.T) {
	_, err := redis.ParseURL("redis://:topsecret@,")
	require.Error(t, err, "空 host 列表应返回错误")
	assert.NotContains(t, err.Error(), "topsecret", "错误消息不应包含 userinfo 密码")
}

// TestParseUrlQueryParams 验证 6 节点集群线上 URL 形态的 query 参数透传：
// 主机 h1:7001 + 5 个 addr= 追加 → Addrs 6 个节点；密码、连接池、超时、重试等
// 参数全部经 ParseClusterURL → setupClusterQueryParams → universalOptionsFromCluster
// 解析后透传到 RedisOptions（内嵌 redis.UniversalOptions）。
func TestParseUrlQueryParams(t *testing.T) {
	// 对应 6 节点集群线上 URL 形态（主机名、密码均为占位符）
	ropt, err := redis.ParseURL(
		"redis://:secret@h1:7001?addr=h2:7002&addr=h3:7003&addr=h4:7004" +
			"&addr=h5:7005&addr=h6:7006" +
			"&pool_size=300&min_idle_conns=20&max_idle_conns=300&max_active_conns=300" +
			"&pool_timeout=3s&conn_max_idle_time=5m" +
			"&max_retries=3&min_retry_backoff=50ms&max_retry_backoff=2s" +
			"&dial_timeout=5s&read_timeout=10s&write_timeout=10s" +
			"&max_redirects=3",
	)
	require.NoError(t, err, "解析 6 节点集群 URL 应成功")

	// Addrs == 6 个节点（host + 5 个 addr= 追加）
	assert.Equal(t, []string{"h1:7001", "h2:7002", "h3:7003", "h4:7004", "h5:7005", "h6:7006"},
		ropt.Addrs, "Addrs 应为 6 节点列表")

	// 密码
	assert.Equal(t, "secret", ropt.Password, "Password 应正确")

	// 连接池参数
	assert.Equal(t, 300, ropt.PoolSize, "PoolSize")
	assert.Equal(t, 20, ropt.MinIdleConns, "MinIdleConns")
	assert.Equal(t, 300, ropt.MaxIdleConns, "MaxIdleConns")
	assert.Equal(t, 300, ropt.MaxActiveConns, "MaxActiveConns")

	// 超时参数
	assert.Equal(t, 3*time.Second, ropt.PoolTimeout, "PoolTimeout")
	assert.Equal(t, 5*time.Minute, ropt.ConnMaxIdleTime, "ConnMaxIdleTime")
	assert.Equal(t, 5*time.Second, ropt.DialTimeout, "DialTimeout")
	assert.Equal(t, 10*time.Second, ropt.ReadTimeout, "ReadTimeout")
	assert.Equal(t, 10*time.Second, ropt.WriteTimeout, "WriteTimeout")

	// 重试参数
	assert.Equal(t, 3, ropt.MaxRetries, "MaxRetries")
	assert.Equal(t, 50*time.Millisecond, ropt.MinRetryBackoff, "MinRetryBackoff")
	assert.Equal(t, 2*time.Second, ropt.MaxRetryBackoff, "MaxRetryBackoff")

	// 集群路由参数
	assert.Equal(t, 3, ropt.MaxRedirects, "MaxRedirects")
}

func TestParseUrl(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		opts       []redis.Option
		wantAddrs  []string
		wantUser   string
		wantPass   string
		wantDB     int
		wantMaster string
		wantErr    bool
	}{
		{
			name:      "单地址无密码",
			url:       "redis://host:6379",
			wantAddrs: []string{"host:6379"},
		},
		{
			name:      "单地址带密码",
			url:       "redis://:secret@host:6379",
			wantAddrs: []string{"host:6379"},
			wantPass:  "secret",
		},
		{
			name:      "单地址带用户名密码",
			url:       "redis://user:secret@host:6379",
			wantAddrs: []string{"host:6379"},
			wantUser:  "user",
			wantPass:  "secret",
		},
		{
			// 集群 ParseClusterURL 不解析 db path（集群仅 db0），DB 保持零值
			name:      "带 db path（集群解析忽略 db）",
			url:       "redis://host:6379/1",
			wantAddrs: []string{"host:6379"},
			wantDB:    0,
		},
		{
			// 核心新增：逗号分隔多地址（Redis Cluster 种子列表）
			name:      "逗号分隔多地址",
			url:       "redis://:secret@h1:7001,h2:7002,h3:7003",
			wantAddrs: []string{"h1:7001", "h2:7002", "h3:7003"},
			wantPass:  "secret",
		},
		{
			name:      "多地址带用户名密码",
			url:       "redis://user:secret@h1:7001,h2:7002",
			wantAddrs: []string{"h1:7001", "h2:7002"},
			wantUser:  "user",
			wantPass:  "secret",
		},
		{
			// 单地址 + master_name：单哨兵节点（failover client）
			name:       "单地址带哨兵 master_name",
			url:        "redis://:pass@host:26379?master_name=mymaster",
			wantAddrs:  []string{"host:26379"},
			wantPass:   "pass",
			wantMaster: "mymaster",
		},
		{
			// 核心新增：多地址 + master_name → 哨兵节点列表
			name:       "逗号多地址带哨兵 master_name",
			url:        "redis://:pass@h1:26379,h2:26379,h3:26379?master_name=mm",
			wantAddrs:  []string{"h1:26379", "h2:26379", "h3:26379"},
			wantPass:   "pass",
			wantMaster: "mm",
		},
		{
			// 剥离只删 master_name，不误伤其他官方参数（read_timeout/addr）
			name:       "多地址带 master_name 与官方参数混用",
			url:        "redis://:pass@h1:26379,h2:26379?master_name=mm&read_timeout=3s&addr=h4:26379",
			wantAddrs:  []string{"h1:26379", "h2:26379", "h4:26379"},
			wantPass:   "pass",
			wantMaster: "mm",
		},
		{
			// 空 master_name 按不存在处理：剥离避免 unexpected option，但不回填
			name:      "空 master_name 按不存在处理",
			url:       "redis://:pass@host:26379?master_name=",
			wantAddrs: []string{"host:26379"},
			wantPass:  "pass",
		},
		{
			name:     "非法 URL",
			url:      "://bad",
			wantErr:  true,
		},
		{
			name:    "空串",
			url:     "",
			wantErr: true,
		},
		{
			// Option 在 URL 解析之后应用，可覆盖 URL 中的密码
			name:      "Option 覆盖 URL 密码",
			url:       "redis://:secret@host:6379",
			opts:      []redis.Option{redis.WithPassword("override")},
			wantAddrs: []string{"host:6379"},
			wantPass:  "override",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ropt, err := redis.ParseURL(c.url, c.opts...)
			if c.wantErr {
				assert.Error(t, err, "应返回错误")
				return
			}

			require.NoError(t, err)
			assert.Equal(t, c.wantAddrs, ropt.Addrs, "Addrs 应正确")
			assert.Equal(t, c.wantUser, ropt.Username, "Username 应正确")
			assert.Equal(t, c.wantPass, ropt.Password, "Password 应正确")
			assert.Equal(t, c.wantDB, ropt.DB, "DB 应正确")
			assert.Equal(t, c.wantMaster, ropt.MasterName, "MasterName 应正确")
		})
	}
}

// TestNewWithClientAddPrefixUsesUcConfig 验证 P0-1 修复：
// NewWithClient 包装外部 uc 后，AddPrefix 派生的子连接池继承 uc 的真实
// 连接配置（连到 miniredis），而不是静默回落到默认的 127.0.0.1:6379。
func TestNewWithClientAddPrefixUsesUcConfig(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	uc := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = uc.Close() }()

	rdb, err := redis.NewWithClient(uc, redis.WithPrefix("app"))
	assert.NoError(t, err)
	defer func() { _ = rdb.GracefulClose(context.Background()) }()

	// 子连接池应连到 uc 的地址（mr），写入落到 app:h1:k
	child := rdb.AddPrefix("h1")
	assert.NoError(t, child.Set(context.Background(), "k", "v", time.Hour).Err())
	val, err := mr.Get("app:h1:k")
	assert.NoError(t, err)
	assert.Equal(t, "v", val)
}

// TestNewWithClientCloseKeepsUc 验证 P1-3 修复：
// redisClient 重写 Close() 后，对 NewWithClient 包装的 client 调 Close()
// 只关闭派生子池，不关闭外部传入的 uc（uc 仍可用）。
func TestNewWithClientCloseKeepsUc(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	uc := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = uc.Close() }()

	rdb, err := redis.NewWithClient(uc, redis.WithPrefix("app"))
	assert.NoError(t, err)

	child := rdb.AddPrefix("h1")
	assert.NoError(t, rdb.Close()) // 重写的 Close：只关子池与自身状态

	// 外部 uc 未被关闭，仍可正常使用
	assert.NoError(t, uc.Ping(context.Background()).Err())

	// 子连接池已随父关闭，其写入不应再成功（连接已关闭）
	err = child.Set(context.Background(), "k", "v", time.Hour).Err()
	assert.Error(t, err, "子连接池应已随父 Close 级联关闭")
}

// TestPipelinePrefix 验证 P2-8：pipeline 场景下每个 key 前缀只加一次，
// 不会重复添加（hook 的 ProcessPipelineHook 对每条命令各调用一次 renameKey）。
func TestPipelinePrefix(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	rdb := redis.New(redis.WithAddr(mr.Addr()), redis.WithPrefix("app"))
	defer func() { _ = rdb.GracefulClose(context.Background()) }()

	ctx := context.Background()
	pipe := rdb.Pipeline()
	pipe.Set(ctx, "k1", "v1", 0)
	pipe.Set(ctx, "k2", "v2", 0)
	pipe.Del(ctx, "k1")
	_, err = pipe.Exec(ctx)
	assert.NoError(t, err)

	// 前缀只加一次：k1 在 pipeline 中被 Set 后又 Del（app:k1 不存在），
	// k2 保留（app:k2 值为 v2）；不存在 app:app: 二次前缀。
	assert.False(t, mr.Exists("app:k1"), "app:k1 已被 pipeline 中的 Del 删除")
	assert.True(t, mr.Exists("app:k2"), "app:k2 应存在")
	v2, err := mr.Get("app:k2")
	assert.NoError(t, err)
	assert.Equal(t, "v2", v2)
	assert.False(t, mr.Exists("app:app:k2"), "不应出现二次前缀")
}

// TestSubscribeWithPrefix 验证 P1-1：PrefixHook 独立用法下，
// Publish 端经 hook 加前缀，SubscribeWithPrefix 订阅端显式加前缀，两端对称互通。
// 注意：miniredis v2.5 不支持 PUBLISH/SUBSCRIBE，需真实 Redis（REDIS_URL），
// 未设置环境时跳过。
func TestSubscribeWithPrefix(t *testing.T) {
	if os.Getenv("REDIS_URL") == "" {
		t.Skip("REDIS_URL 未设置，跳过（miniredis 不支持 pubsub，需真实 Redis）")
	}

	url := os.Getenv("REDIS_URL")

	// REDIS_URL 可能是逗号分隔的集群种子地址列表，而单机 goredis.ParseURL
	// 不支持逗号分隔 host，这里取第一个地址建立单机连接（集群 pubsub 为
	// 全节点广播，任意节点订阅即可收到）。
	firstURL, err := firstAddrURL(url)
	if err != nil {
		t.Fatal(err)
	}
	opt, err := goredis.ParseURL(firstURL)
	if err != nil {
		t.Fatal(err)
	}
	uc := goredis.NewClient(opt)
	defer func() { _ = uc.Close() }()
	uc.AddHook(redis.PrefixHook("app", ":"))

	ctx := context.Background()
	sub := redis.SubscribeWithPrefix(uc, "app", ":", "chan1")
	defer func() { _ = sub.Close() }()

	// 订阅建立是异步的：循环发布+接收直到成功，避免竞态。
	// Publish 经 hook 加到 "app:chan1"，SubscribeWithPrefix 订阅的也是 "app:chan1"。
	// 注意 ReceiveTimeout 可能先返回订阅确认等非 Message 消息，需类型断言过滤。
	assert.Eventually(t, func() bool {
		if err := uc.Publish(ctx, "chan1", "hello").Err(); err != nil {
			return false
		}
		raw, err := sub.ReceiveTimeout(ctx, 100*time.Millisecond)
		if err != nil {
			return false
		}
		msg, ok := raw.(*goredis.Message)
		return ok && msg.Payload == "hello"
	}, 5*time.Second, 100*time.Millisecond)
}

// TestNewBloomFilterWithEstimateViaInterface 验证 P2-1：
// NewBloomFilterWithEstimate 已加入 Client 接口，miniredis 走 bitmap 实现。
func TestNewBloomFilterWithEstimateViaInterface(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		bf := rdb.NewBloomFilterWithEstimate("bfkey", 1000, 0.01)
		ctx := context.Background()

		added, err := bf.Add(ctx, "item1")
		assert.NoError(t, err)
		assert.True(t, added)

		exists, err := bf.Exists(ctx, "item1")
		assert.NoError(t, err)
		assert.True(t, exists)
	})
}

// TestCapabilityModules 验证单一能力判定接口在 miniredis 上的行为：
// miniredis 不加载任何模块，因此各 HasXXX 判定均应返回 false。
func TestCapabilityModules(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		cap := rdb.Capability()
		assert.False(t, cap.HasBloom(), "miniredis 不应具备 Bloom 模块")
		assert.False(t, cap.HasCMS(), "miniredis 不应具备 CMS 模块")
		assert.False(t, cap.HasCuckoo(), "miniredis 不应具备 Cuckoo 模块")
		assert.False(t, cap.HasJSON(), "miniredis 不应具备 ReJSON 模块")
		assert.False(t, cap.HasSearch(), "miniredis 不应具备 Search 模块")
		assert.False(t, cap.HasTimeSeries(), "miniredis 不应具备 TimeSeries 模块")
		assert.False(t, cap.HasTopK(), "miniredis 不应具备 TopK 模块")
		assert.False(t, cap.HasTDigest(), "miniredis 不应具备 TDigest 模块")
		assert.False(t, cap.HasGraph(), "miniredis 不应具备 Graph 模块")
		assert.False(t, cap.HasModule("bf"), "HasModule 不应命中未加载模块")
	})
}

func TestBf(t *testing.T) {
	test.RunOnRedisStack(t, func(rdb redis.Client) {
		key := "ffff"
		rdb.Del(context.Background(), key)

		rdb.CFReserve(context.Background(), "ccc", 1000000)

		if err := rdb.BFReserve(context.Background(), "ffff", 0.01, 1000000).Err(); err != nil {
			t.Fatal(err)
		}

		for i := 0; i < 10000; i++ {
			rdb.BFAdd(context.Background(), "ffff", i)
		}
	})
}

func BenchmarkBF(b *testing.B) {
	key := "abcdef"

	test.RunOnRedisStack(b, func(rdb redis.Client) {
		rdb.BFReserve(context.Background(), key, 0.0001, 100000)
		ctx := context.Background()

		b.Run("bf", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				rdb.BFExists(ctx, key, randomHex(1))
			}
		})
	})

}
func TestRateLimiter(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		if err := rdb.FlushDB(context.Background()).Err(); err != nil {
			panic(err)
		}

		// 通过 Client 接口调用 NewRateLimiter（空名称：不隔离，行为与旧版一致）
		limiter := rdb.NewRateLimiter("")
		for i := 0; i < 3; i++ {
			res, err := limiter.Allow(context.Background(), "project:123", 10)
			if err != nil {
				panic(err)
			}

			fmt.Println("allowed", res.Allowed, "remaining", res.Remaining)
		}

	})

}

// TestRateLimiterNameIsolation 验证按名称隔离限流 key 空间：
// 不同 name 的限流器对相同业务 key 互不影响（各自独立配额）。
func TestRateLimiterNameIsolation(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		ctx := context.Background()
		require.NoError(t, rdb.FlushDB(ctx).Err(), "清空限流相关 key")

		la := rdb.NewRateLimiter("a")
		lb := rdb.NewRateLimiter("b")

		// a 限制 2 次/秒：第 1、2 次放行，第 3 次应被拒
		for i := 0; i < 2; i++ {
			res, err := la.Allow(ctx, "key", 2)
			require.NoError(t, err)
			assert.True(t, res.Allowed, "a 第 %d 次应放行", i+1)
		}
		res, err := la.Allow(ctx, "key", 2)
		require.NoError(t, err)
		assert.False(t, res.Allowed, "a 第 3 次应被拒")

		// b 的相同业务 key 独立配额：第 1 次应放行（不受 a 影响）
		res, err = lb.Allow(ctx, "key", 2)
		require.NoError(t, err)
		assert.True(t, res.Allowed, "b 的同 key 应独立放行")

		// 同一 name 内相同 key 共享配额：a 继续被拒（共享 a 的配额）
		res, err = la.Allow(ctx, "key", 2)
		require.NoError(t, err)
		assert.False(t, res.Allowed, "a 内相同 key 应共享配额")
	})
}

// TestRateLimiterKeyNamespace 验证名称隔离后的实际 Redis key 含命名空间：
// redis_rate 存储 key = "rate:" + 传入 key（Lua 脚本 KEYS[1]，见 redis_rate
// 源码 redisPrefix 常量），名称隔离后应分别为 "rate:a:key" 与 "rate:b:key"。
func TestRateLimiterKeyNamespace(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.New(redis.WithAddr(mr.Addr()))
	defer func() { _ = rdb.GracefulClose(context.Background()) }()
	ctx := context.Background()

	la := rdb.NewRateLimiter("a")
	lb := rdb.NewRateLimiter("b")

	_, err = la.Allow(ctx, "key", 10)
	require.NoError(t, err)
	_, err = lb.Allow(ctx, "key", 10)
	require.NoError(t, err)

	// 命名空间 key 独立存在，未隔离的 "rate:key" 不存在
	assert.True(t, mr.Exists("rate:a:key"), "限流 key 应含命名空间 a")
	assert.True(t, mr.Exists("rate:b:key"), "限流 key 应含命名空间 b")
	assert.False(t, mr.Exists("rate:key"), "不应存在未隔离的 rate:key")
}

// TestNewBloomFilterViaInterface 验证通过 Client 接口调用 NewBloomFilter：
// miniredis 无 bf 模块，走 bitmap 回退实现，验证接口动态分派正确。
func TestNewBloomFilterViaInterface(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		bf := rdb.NewBloomFilter("bfkey")
		ctx := context.Background()

		added, err := bf.Add(ctx, "item1")
		assert.NoError(t, err)
		assert.True(t, added)

		exists, err := bf.Exists(ctx, "item1")
		assert.NoError(t, err)
		assert.True(t, exists)

		// 重复添加：item 已存在，返回 false
		added, err = bf.Add(ctx, "item1")
		assert.NoError(t, err)
		assert.False(t, added)
	})
}
