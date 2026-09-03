// 端到端测试：内嵌 nats-server（test-only），每个用例独立 server，无外部依赖。
// 验收标准之一：nats.go 插件主代码 import 清单不含 nats-server。
package nats_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charlienet/gadget/broker"
	plugin "github.com/charlienet/gadget/plugins/broker/nats"
	"github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startServer 启动一个随机端口的内嵌 nats-server，测试结束自动关闭。
func startServer(t *testing.T) *server.Server {
	t.Helper()
	srv, err := server.NewServer(&server.Options{Port: -1, Host: "127.0.0.1"})
	require.NoError(t, err, "内嵌 nats-server 创建失败")
	srv.Start()
	require.True(t, srv.ReadyForConnections(5*time.Second), "内嵌 nats-server 未就绪")
	t.Cleanup(srv.Shutdown)
	return srv
}

// 用例 1：Subscribe 后 Publish，handler 必须收到；Topic==subject、Body 相等。
func TestSubscribeReceivesPublishedMessage(t *testing.T) {
	srv := startServer(t)
	b, err := plugin.New(plugin.WithURL(srv.ClientURL()), plugin.WithName("case1"))
	require.NoError(t, err)
	defer func() { assert.NoError(t, b.Close()) }()

	done := make(chan broker.Event, 8)
	sub, err := b.Subscribe("case1.basic", func(e broker.Event) error {
		done <- e
		return nil
	})
	require.NoError(t, err)
	defer func() { assert.NoError(t, sub.Unsubscribe()) }()

	require.NoError(t, b.Publish("case1.basic", &broker.Message{Body: "hello"}))

	select {
	case e := <-done:
		assert.Equal(t, "case1.basic", e.Topic())
		require.NotNil(t, e.Message())
		assert.Equal(t, "hello", e.Message().Body)
		assert.NoError(t, e.Ack())   // 接口占位：恒 nil
		assert.NoError(t, e.Error()) // handler 错误不经 Event 回流：恒 nil
	case <-time.After(5 * time.Second):
		t.Fatal("等待 handler 收到消息超时（缺陷 1 回归）")
	}
}

// 用例 2：通配符订阅 foo.* 收到 foo.bar 时，Event.Topic() 必须是真实 subject。
func TestSubscribeWildcardSubjectReportsRealTopic(t *testing.T) {
	srv := startServer(t)
	b, err := plugin.New(plugin.WithURL(srv.ClientURL()))
	require.NoError(t, err)
	defer func() { assert.NoError(t, b.Close()) }()

	done := make(chan string, 8)
	_, err = b.Subscribe("foo.*", func(e broker.Event) error {
		done <- e.Topic()
		return nil
	})
	require.NoError(t, err)

	require.NoError(t, b.Publish("foo.bar", &broker.Message{Body: "wild"}))

	select {
	case topic := <-done:
		assert.Equal(t, "foo.bar", topic, "通配符订阅必须报告真实 subject")
	case <-time.After(5 * time.Second):
		t.Fatal("等待通配符订阅消息超时")
	}
}

// 用例 3：连接不可达地址时 New 必须返回 error 且 Broker 为 nil（缺陷 2 回归）。
func TestNewFailsOnUnreachableURL(t *testing.T) {
	b, err := plugin.New(
		plugin.WithURL("nats://127.0.0.1:1"),
		// 防御性收敛：禁掉默认重连，避免任何重试路径拖慢用例
		plugin.WithConnOptions(natsgo.MaxReconnects(0), natsgo.ReconnectWait(time.Hour)),
	)
	require.Error(t, err, "连接失败必须返回 error")
	assert.Nil(t, b, "连接失败必须返回 nil Broker")
	t.Logf("连接失败错误: %v", err)
}

// 用例 4：Close 之后 Publish 的错误透传 nats 库语义错误。
// 实测锁定（nats.go v1.37）：Drain 异步，Close 返回时连接可能仍在向 CLOSED
// 过渡，故经 ClosedCB 等到连接真正关闭后再断言终态错误。
func TestPublishAfterCloseReturnsError(t *testing.T) {
	srv := startServer(t)
	closed := make(chan struct{})
	b, err := plugin.New(
		plugin.WithURL(srv.ClientURL()),
		plugin.WithConnOptions(natsgo.ClosedHandler(func(*natsgo.Conn) { close(closed) })),
	)
	require.NoError(t, err)

	require.NoError(t, b.Close())

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("等待连接在 Drain 后完全关闭超时")
	}

	err = b.Publish("case4.after-close", &broker.Message{Body: "x"})
	require.Error(t, err)
	assert.ErrorIs(t, err, natsgo.ErrConnectionClosed)
}

// 用例 5：WithSyncPublish 下，服务端在线时 Publish 返回 nil；
// server shutdown 后走 FlushTimeout 路径返回 error。
func TestSyncPublishConfirmsServerReceipt(t *testing.T) {
	srv := startServer(t)
	b, err := plugin.New(
		plugin.WithURL(srv.ClientURL()),
		plugin.WithSyncPublish(time.Second),
		plugin.WithConnOptions(natsgo.MaxReconnects(0)),
	)
	require.NoError(t, err)

	// 服务端在线：Publish + FlushTimeout 往返确认成功
	require.NoError(t, b.Publish("case5.sync", &broker.Message{Body: "confirm-me"}))

	// 服务端关闭：同步确认路径必须报错
	srv.Shutdown()
	err = b.Publish("case5.sync", &broker.Message{Body: "x"})
	assert.Error(t, err, "server shutdown 后同步发布必须返回 error（FlushTimeout 路径）")

	// 此处 Close 已无健康连接可 drain，容忍错误仅记录
	if cerr := b.Close(); cerr != nil {
		t.Logf("server shutdown 后 Close: %v", cerr)
	}
}

// 用例 6：handler panic 被 recover 转 error 经 hook 上报；进程不崩、订阅存活。
func TestHandlerPanicRecoveredAndReported(t *testing.T) {
	srv := startServer(t)
	hooks := make(chan error, 8)
	topics := make(chan string, 8)
	b, err := plugin.New(
		plugin.WithURL(srv.ClientURL()),
		plugin.WithHandlerErrorHandler(func(topic string, herr error) {
			topics <- topic
			hooks <- herr
		}),
	)
	require.NoError(t, err)
	defer func() { assert.NoError(t, b.Close()) }()

	delivered := make(chan string, 8)
	_, err = b.Subscribe("case6.panic", func(e broker.Event) error {
		if e.Message().Body == "boom" {
			panic("boom")
		}
		delivered <- e.Message().Body
		return nil
	})
	require.NoError(t, err)

	require.NoError(t, b.Publish("case6.panic", &broker.Message{Body: "boom"}))
	select {
	case herr := <-hooks:
		require.NotNil(t, herr)
		assert.Contains(t, herr.Error(), "panicked")
		assert.Contains(t, herr.Error(), "boom")
		assert.Equal(t, "case6.panic", <-topics)
	case <-time.After(5 * time.Second):
		t.Fatal("等待 panic 转译上报超时（进程未崩溃即 recover 生效的前提）")
	}

	// 后续消息仍投递：订阅在 handler panic 后存活
	require.NoError(t, b.Publish("case6.panic", &broker.Message{Body: "alive"}))
	select {
	case body := <-delivered:
		assert.Equal(t, "alive", body, "handler panic 后订阅必须存活")
	case <-time.After(5 * time.Second):
		t.Fatal("handler panic 后订阅未收到后续消息")
	}
}

// 用例 7：handler 返回 error 经 hook 收到原 error + 正确 topic；无 hook 时静默丢弃、
// 不 panic 不阻塞后续投递。
func TestHandlerErrorReported(t *testing.T) {
	srv := startServer(t)

	type hookRecord struct {
		topic string
		err   error
	}
	hooks := make(chan hookRecord, 8)
	b, err := plugin.New(
		plugin.WithURL(srv.ClientURL()),
		plugin.WithHandlerErrorHandler(func(topic string, herr error) {
			hooks <- hookRecord{topic: topic, err: herr}
		}),
	)
	require.NoError(t, err)
	defer func() { assert.NoError(t, b.Close()) }()

	wantErr := errors.New("handler failed")
	_, err = b.Subscribe("case7.err", func(e broker.Event) error {
		return wantErr
	})
	require.NoError(t, err)

	require.NoError(t, b.Publish("case7.err", &broker.Message{Body: "x"}))
	select {
	case rec := <-hooks:
		assert.ErrorIs(t, rec.err, wantErr, "hook 必须收到 handler 返回的原始 error")
		assert.Equal(t, "case7.err", rec.topic)
	case <-time.After(5 * time.Second):
		t.Fatal("等待 handler 错误上报超时")
	}

	// 无 hook：错误丢弃但不得 panic / 阻塞后续消息投递
	b2, err := plugin.New(plugin.WithURL(srv.ClientURL()))
	require.NoError(t, err)
	defer func() { assert.NoError(t, b2.Close()) }()

	count := make(chan struct{}, 8)
	_, err = b2.Subscribe("case7.nohook", func(e broker.Event) error {
		count <- struct{}{}
		return errors.New("dropped silently")
	})
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		require.NoError(t, b2.Publish("case7.nohook", &broker.Message{Body: "x"}))
	}
	for i := 0; i < 2; i++ {
		select {
		case <-count:
		case <-time.After(5 * time.Second):
			t.Fatalf("无 hook 时第 %d 条消息投递被阻塞", i+1)
		}
	}
}

// 用例 8：Unsubscribe 返回后再 Publish，计数恒为 N（不再投递）。
func TestUnsubscribeStopsDelivery(t *testing.T) {
	srv := startServer(t)
	b, err := plugin.New(plugin.WithURL(srv.ClientURL()))
	require.NoError(t, err)
	defer func() { assert.NoError(t, b.Close()) }()

	const n = 3
	got := make(chan struct{}, 64)
	sub, err := b.Subscribe("case8.unsub", func(e broker.Event) error {
		got <- struct{}{}
		return nil
	})
	require.NoError(t, err)

	for i := 0; i < n; i++ {
		require.NoError(t, b.Publish("case8.unsub", &broker.Message{Body: "x"}))
	}
	for i := 0; i < n; i++ {
		select {
		case <-got:
		case <-time.After(5 * time.Second):
			t.Fatalf("Unsubscribe 前仅收到 %d/%d 条", i, n)
		}
	}

	require.NoError(t, sub.Unsubscribe())

	// 同步点：Unsubscribe 返回后再 publish
	for i := 0; i < n; i++ {
		require.NoError(t, b.Publish("case8.unsub", &broker.Message{Body: "y"}))
	}
	select {
	case <-got:
		t.Fatal("Unsubscribe 之后仍在收到消息")
	case <-time.After(500 * time.Millisecond):
		// 静默窗口：无事发生为期望行为
	}
	// 已消费恰 n 条且再无新增：总投递计数恒为 N
	assert.Equal(t, 0, len(got))
}

// 用例 9：publish N 条后立即 Close，Drain 保证在途消息全部送达；二次 Close 返回 nil。
func TestCloseDrainsInflight(t *testing.T) {
	srv := startServer(t)
	b, err := plugin.New(plugin.WithURL(srv.ClientURL()))
	require.NoError(t, err)

	const n = 5
	got := make(chan struct{}, n)
	_, err = b.Subscribe("case9.drain", func(e broker.Event) error {
		got <- struct{}{}
		return nil
	})
	require.NoError(t, err)

	for i := 0; i < n; i++ {
		require.NoError(t, b.Publish("case9.drain", &broker.Message{Body: "m"}))
	}
	// publish 后立即 Close：Drain 排空在途消息
	require.NoError(t, b.Close())

	received := 0
	deadline := time.After(5 * time.Second)
	for received < n {
		select {
		case <-got:
			received++
		case <-deadline:
			t.Fatalf("Drain 不完：仅收到 %d/%d 条在途消息", received, n)
		}
	}

	assert.NoError(t, b.Close(), "第二次 Close 必须返回 nil（幂等）")
}

// 用例 10：多 goroutine 混合 Subscribe/Unsubscribe/Publish，-race 无告警，Close 干净退出。
func TestConcurrentSubscribeUnsubscribePublish(t *testing.T) {
	srv := startServer(t)
	b, err := plugin.New(plugin.WithURL(srv.ClientURL()), plugin.WithName("case10"))
	require.NoError(t, err)

	var (
		wg               sync.WaitGroup
		received         int64
		publishedSuccess int64
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			topic := fmt.Sprintf("case10.mix.%d", idx)
			for j := 0; j < 20; j++ {
				sub, serr := b.Subscribe(topic, func(e broker.Event) error {
					atomic.AddInt64(&received, 1)
					return nil
				})
				if serr != nil {
					t.Logf("并发 Subscribe 返回错误: %v", serr)
					return
				}
				if uerr := sub.Unsubscribe(); uerr != nil {
					t.Logf("并发 Unsubscribe 返回错误: %v", uerr)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if perr := b.Publish(fmt.Sprintf("case10.mix.%d", j%4), &broker.Message{Body: "x"}); perr == nil {
					atomic.AddInt64(&publishedSuccess, 1)
				}
			}
		}()
	}
	wg.Wait()

	assert.NoError(t, b.Close(), "Close 必须干净退出")
	assert.Greater(t, atomic.LoadInt64(&publishedSuccess), int64(0))
	// received 计数取决于并发时序，本用例核心断言是 -race 无告警且不死锁
}

// 用例 11：非法 Option 值在 New 内 panic，文案含 Option 名。
func TestNewPanicsOnInvalidOptions(t *testing.T) {
	tests := []struct {
		name      string
		opt       plugin.Option
		wantPanic string
	}{
		{"WithURL_empty_url", plugin.WithURL(""), "WithURL"},
		{"WithName_empty_name", plugin.WithName(""), "WithName"},
		{"WithSyncPublish_zero", plugin.WithSyncPublish(0), "WithSyncPublish"},
		{"WithSyncPublish_negative", plugin.WithSyncPublish(-time.Second), "WithSyncPublish"},
		{"WithHandlerErrorHandler_nil", plugin.WithHandlerErrorHandler(nil), "WithHandlerErrorHandler"},
		{"WithConnOptions_nil_element", plugin.WithConnOptions(nil), "WithConnOptions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				require.NotNil(t, r, "非法 Option 必须 panic")
				assert.Contains(t, fmt.Sprint(r), tt.wantPanic, "panic 文案必须含 Option 名")
			}()
			plugin.New(tt.opt)
			t.Fatal("非法 Option 未触发 panic")
		})
	}
}
