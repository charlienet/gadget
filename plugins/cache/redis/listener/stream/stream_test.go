package stream

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/redis/test"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

// TestInstanceSuffix 验证实例标识稳定且非空（hostname-pid）。
func TestInstanceSuffix(t *testing.T) {
	s1 := instanceSuffix()
	s2 := instanceSuffix()

	assert.NotEmpty(t, s1)
	assert.Equal(t, s1, s2, "同进程内 instanceSuffix 应稳定（hostname-pid）")
}

// TestWithConsumerGroupSuffix 验证 F3：WithConsumerGroup 作为组名“前缀”，
// 最终 group 名必须拼接实例唯一后缀——每实例独立 consumer group，
// 才能保证缓存失效广播到所有实例（而非同组消费分摊）。
func TestWithConsumerGroupSuffix(t *testing.T) {
	s := &streamListener{}
	WithConsumerGroup("my-group")(s)

	assert.Equal(t, "my-group-"+instanceSuffix(), s.group)
	assert.NotEqual(t, "my-group", s.group)
}

// TestWithPublishTimeout 验证 F4：Publish（XAdd）超时可配置。
func TestWithPublishTimeout(t *testing.T) {
	s := &streamListener{publishTimeout: defaultPublishTimeout}
	WithPublishTimeout(5 * time.Second)(s)

	assert.Equal(t, 5*time.Second, s.publishTimeout)
}

// ---------------------------------------------------------------------------
// watch 核心路径测试
//
// 说明：本模块依赖的 miniredis v2.5.0 不支持 stream 相关命令
// （XADD/XREADGROUP/XGROUP*/XACK 均未实现），因此无法用 RunOnMiniRedis
// 做本地端到端验证。这里用最小 mock（仅覆盖 watch 用到的 4 个命令，
// 其余方法内嵌 nil 接口，不会被调用）驱动 watch 的核心逻辑：
// NOGROUP 按需创建、消息投递 + ACK、失败退避重试。
// 真实 Redis 端到端链路见 TestStreamWatchIntegration（带 skip 守卫）。
// ---------------------------------------------------------------------------

// mockRedis 最小化 mock：只实现 watch 用到的 stream 命令，
// 其余方法由内嵌的 redis.Client（nil 接口）承接——不会被调用。
//
// AddPrefix 派生 id+1 的副本（模拟真实 client 的"新连接池"语义），
// readClientID 用于观测最近一次 XReadGroup 调用所在 client 实例，
// 便于测试验证 Initialize 后 watch 是否切换到带前缀的新 client。
type mockRedis struct {
	redis.Client
	id            int
	readClientID  *int32 // 记录最近一次 XReadGroup 调用的 client 实例 id（仅测试观测用）
	readGroup     func(ctx context.Context, a *goredis.XReadGroupArgs) *goredis.XStreamSliceCmd
	createGroup   func(ctx context.Context, stream, group, start string) *goredis.StatusCmd
	createGroupMk func(ctx context.Context, stream, group, start string) *goredis.StatusCmd
	ack           func(ctx context.Context, stream, group string, ids ...string) *goredis.IntCmd
	add           func(ctx context.Context, a *goredis.XAddArgs) *goredis.StringCmd
}

func (m *mockRedis) AddPrefix(prefix ...string) redis.Client {
	next := *m
	next.id = m.id + 1
	return &next
}

func (m *mockRedis) XReadGroup(ctx context.Context, a *goredis.XReadGroupArgs) *goredis.XStreamSliceCmd {
	if m.readClientID != nil {
		atomic.StoreInt32(m.readClientID, int32(m.id))
	}
	if m.readGroup != nil {
		return m.readGroup(ctx, a)
	}
	return goredis.NewXStreamSliceCmdResult(nil, errors.New("mock: XReadGroup not configured"))
}

func (m *mockRedis) XGroupCreate(ctx context.Context, stream, group, start string) *goredis.StatusCmd {
	if m.createGroup != nil {
		return m.createGroup(ctx, stream, group, start)
	}
	cmd := goredis.NewStatusCmd(ctx)
	cmd.SetErr(errors.New("mock: XGroupCreate not configured"))
	return cmd
}

func (m *mockRedis) XGroupCreateMkStream(ctx context.Context, stream, group, start string) *goredis.StatusCmd {
	if m.createGroupMk != nil {
		return m.createGroupMk(ctx, stream, group, start)
	}
	cmd := goredis.NewStatusCmd(ctx)
	cmd.SetErr(errors.New("mock: XGroupCreateMkStream not configured"))
	return cmd
}

func (m *mockRedis) XAck(ctx context.Context, stream, group string, ids ...string) *goredis.IntCmd {
	if m.ack != nil {
		return m.ack(ctx, stream, group, ids...)
	}
	cmd := goredis.NewIntCmd(ctx)
	cmd.SetErr(errors.New("mock: XAck not configured"))
	return cmd
}

func (m *mockRedis) XAdd(ctx context.Context, a *goredis.XAddArgs) *goredis.StringCmd {
	if m.add != nil {
		return m.add(ctx, a)
	}
	cmd := goredis.NewStringCmd(ctx)
	cmd.SetErr(errors.New("mock: XAdd not configured"))
	return cmd
}

// redisErr 模拟 Redis 服务端返回的错误：实现 go-redis 的 Error 接口
// （带 RedisError 标记方法），使 watch 中的 HasErrorPrefix(err, "NOGROUP")
// 能正确识别——普通 errors.New 不实现该接口，无法触发 NOGROUP 分支。
type redisErr string

func (e redisErr) Error() string { return string(e) }
func (redisErr) RedisError()     {}

// TestStreamWatchNoGroupCreate 验证 watch 对 NOGROUP 的兜底：
// group/stream 尚不存在（未经 Initialize）时，首次 XReadGroup 报 NOGROUP，
// watch 应调用 XGroupCreateMkStream 按需创建并继续读取。
func TestStreamWatchNoGroupCreate(t *testing.T) {
	ctx := context.Background()

	var (
		readCalls   int32
		createCalls int32
	)
	m := &mockRedis{
		readGroup: func(ctx context.Context, a *goredis.XReadGroupArgs) *goredis.XStreamSliceCmd {
			switch atomic.AddInt32(&readCalls, 1) {
			case 1:
				// 首次读取：stream/group 不存在
				return goredis.NewXStreamSliceCmdResult(nil,
					redisErr("NOGROUP No such key 'cache:invalidate' or consumer group 'g'"))
			default:
				// 创建成功后正常读取（无新消息）
				return goredis.NewXStreamSliceCmdResult(nil, nil)
			}
		},
		createGroupMk: func(ctx context.Context, stream, group, start string) *goredis.StatusCmd {
			atomic.AddInt32(&createCalls, 1)
			cmd := goredis.NewStatusCmd(ctx)
			cmd.SetVal("OK")
			return cmd
		},
	}

	s := NewStreamListener(m)
	defer func() { _ = s.Close(ctx) }()

	// 等待 watch 触发 NOGROUP → 按需创建
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&createCalls) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timeout: XGroupCreateMkStream not called after NOGROUP")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// NOGROUP 按需创建成功即触发就绪信号（Ready 语义：首次成功建立消费）
	select {
	case <-s.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: ready not signaled after NOGROUP create")
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&createCalls), "NOGROUP 后 group 应只创建一次")
	assert.GreaterOrEqual(t, atomic.LoadInt32(&readCalls), int32(2), "创建成功后 watch 应继续读取")
}

// TestStreamWatchDeliverAndAck 验证消息投递与 ACK：
// XReadGroup 返回的消息写入 Subscribe 通道，投递后调用 XAck 确认。
func TestStreamWatchDeliverAndAck(t *testing.T) {
	ctx := context.Background()

	stream := goredis.XStream{
		Stream: "cache:invalidate",
		Messages: []goredis.XMessage{
			{ID: "1-0", Values: map[string]interface{}{"key": "k1"}},
			{ID: "2-0", Values: map[string]interface{}{"key": "k2"}},
		},
	}

	var (
		readCalls int32
		ackedMu   sync.Mutex
		acked     []string
	)
	m := &mockRedis{
		readGroup: func(ctx context.Context, a *goredis.XReadGroupArgs) *goredis.XStreamSliceCmd {
			if atomic.AddInt32(&readCalls, 1) == 1 {
				return goredis.NewXStreamSliceCmdResult([]goredis.XStream{stream}, nil)
			}
			return goredis.NewXStreamSliceCmdResult(nil, nil)
		},
		ack: func(ctx context.Context, stream, group string, ids ...string) *goredis.IntCmd {
			ackedMu.Lock()
			acked = append(acked, ids...)
			ackedMu.Unlock()
			cmd := goredis.NewIntCmd(ctx)
			cmd.SetVal(int64(len(ids)))
			return cmd
		},
	}

	s := NewStreamListener(m)
	defer func() { _ = s.Close(ctx) }()

	// 收到投递的两条消息
	got := make([]string, 0, 2)
	deadline := time.Now().Add(2 * time.Second)
	for len(got) < 2 {
		select {
		case k := <-s.Subscribe():
			got = append(got, k)
		case <-time.After(100 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatalf("timeout: got %v, want [k1 k2]", got)
			}
		}
	}
	assert.Equal(t, []string{"k1", "k2"}, got)

	// 投递后 watch 应异步调用 XAck（携带对应消息 ID）
	deadline = time.Now().Add(2 * time.Second)
	for {
		ackedMu.Lock()
		n := len(acked)
		ackedMu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout: XAck not called for delivered messages")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ackedMu.Lock()
	assert.ElementsMatch(t, []string{"1-0", "2-0"}, acked)
	ackedMu.Unlock()
}

// TestStreamWatchRetryBackoff 验证失败退避重试：
// 首次 XReadGroup 失败（非 NOGROUP，如连接异常）后，watch 按退避延迟
// 重试；恢复后消息正常投递。
func TestStreamWatchRetryBackoff(t *testing.T) {
	ctx := context.Background()
	start := time.Now()

	var readCalls int32
	m := &mockRedis{
		readGroup: func(ctx context.Context, a *goredis.XReadGroupArgs) *goredis.XStreamSliceCmd {
			switch atomic.AddInt32(&readCalls, 1) {
			case 1:
				return goredis.NewXStreamSliceCmdResult(nil, errors.New("mock: connection refused"))
			case 2:
				return goredis.NewXStreamSliceCmdResult([]goredis.XStream{{
					Stream: "cache:invalidate",
					Messages: []goredis.XMessage{
						{ID: "1-0", Values: map[string]interface{}{"key": "after-backoff"}},
					},
				}}, nil)
			default:
				return goredis.NewXStreamSliceCmdResult(nil, nil)
			}
		},
	}

	s := NewStreamListener(m)
	defer func() { _ = s.Close(ctx) }()

	select {
	case k := <-s.Subscribe():
		assert.Equal(t, "after-backoff", k)
		elapsed := time.Since(start)
		// retryBaseDelay=1s：首次失败后应至少等待一个退避周期再重试
		assert.GreaterOrEqual(t, elapsed, retryBaseDelay-100*time.Millisecond,
			"首次失败后应按退避延迟重试")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: message not delivered after backoff retry")
	}

	assert.GreaterOrEqual(t, atomic.LoadInt32(&readCalls), int32(2), "失败后应重新发起读取")
}

// TestStreamWatchInitializeSwapClient 验证 Initialize 与 watch 的并发安全：
// Initialize 在 watch goroutine 启动后才被调用（cache.New 内同步执行），
// 会通过 AddPrefix 替换 s.rdb；watch 每轮读取前取 rdb 快照，应在不产生
// 数据竞态（-race 验证）的前提下切换到带前缀的新 client。
func TestStreamWatchInitializeSwapClient(t *testing.T) {
	ctx := context.Background()

	var lastClientID int32 // 最近一次 XReadGroup 调用所在 client 实例 id
	m := &mockRedis{
		readClientID: &lastClientID,
		readGroup: func(ctx context.Context, a *goredis.XReadGroupArgs) *goredis.XStreamSliceCmd {
			// 无新消息，快速循环，等待 Initialize 替换 client
			return goredis.NewXStreamSliceCmdResult(nil, nil)
		},
	}

	s := NewStreamListener(m)
	defer func() { _ = s.Close(ctx) }()

	// watch 启动后调用 Initialize（模拟 cache.New → Options.init → Initialize）
	time.Sleep(50 * time.Millisecond)
	if i, ok := s.(interface{ Initialize(cache.Options) }); ok {
		i.Initialize(cache.Options{Name: "users"})
	}

	// 等待 watch 切换到 AddPrefix 派生（id=1）的 client
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&lastClientID) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: watch still using client id=%d, want 1 (post-Initialize)",
				atomic.LoadInt32(&lastClientID))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStreamWatchReady 验证 Ready 就绪信号：watch 首次 XReadGroup 成功
// （消费链路建立）即 close 就绪信号。
func TestStreamWatchReady(t *testing.T) {
	ctx := context.Background()

	m := &mockRedis{
		readGroup: func(ctx context.Context, a *goredis.XReadGroupArgs) *goredis.XStreamSliceCmd {
			// 读取成功（无新消息也视为消费链路建立）
			return goredis.NewXStreamSliceCmdResult(nil, nil)
		},
	}

	s := NewStreamListener(m)
	defer func() { _ = s.Close(ctx) }()

	select {
	case <-s.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: ready not signaled after first successful read")
	}
}

// TestStreamWatchReadyNotBeforeSuccess 验证 Ready 在 watch 成功建立消费前
// 不会关闭：XReadGroup 持续失败（Redis 不可达等）时就绪信号保持未触发。
func TestStreamWatchReadyNotBeforeSuccess(t *testing.T) {
	ctx := context.Background()

	m := &mockRedis{
		readGroup: func(ctx context.Context, a *goredis.XReadGroupArgs) *goredis.XStreamSliceCmd {
			return goredis.NewXStreamSliceCmdResult(nil, errors.New("mock: connection refused"))
		},
	}

	s := NewStreamListener(m)
	defer func() { _ = s.Close(ctx) }()

	select {
	case <-s.Ready():
		t.Fatal("ready should not be signaled while reads keep failing")
	case <-time.After(200 * time.Millisecond):
		// 持续失败中：就绪信号未触发 ✓
	}
}

// TestStreamWatchIntegration 端到端验证（需真实 Redis 5.0+，无环境时 skip）：
//   - 未初始化的 group 由 watch 首次读取时的 NOGROUP 检测兜底创建（MKSTREAM）；
//   - XAdd 发布 → watch 消费 → Subscribe 通道收到；
//   - 使用独立 stream/group，避免与其他测试互相干扰。
//
// miniredis v2.5.0 不支持 stream 命令，故本用例必须依赖真实 Redis。
func TestStreamWatchIntegration(t *testing.T) {
	ctx := context.Background()
	streamName := "stream-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)

	test.RunOnRedis(t, func(rdb redis.Client) {
		lis := NewStreamListener(rdb,
			WithStreamName(streamName),
			WithConsumerGroup("it-group"),
		)
		defer func() { _ = lis.Close(ctx) }()

		// 等待 watch 建立（NOGROUP → 创建 group/stream）
		time.Sleep(500 * time.Millisecond)

		assert.NoError(t, lis.Publish("inv-key-1"))
		assert.NoError(t, lis.Publish("inv-key-2"))

		got := make([]string, 0, 2)
		deadline := time.Now().Add(5 * time.Second)
		for len(got) < 2 {
			select {
			case k := <-lis.Subscribe():
				got = append(got, k)
			case <-time.After(200 * time.Millisecond):
				if time.Now().After(deadline) {
					t.Fatalf("timeout waiting delivery: got %v", got)
				}
			}
		}
		assert.ElementsMatch(t, []string{"inv-key-1", "inv-key-2"}, got)
	})
}
