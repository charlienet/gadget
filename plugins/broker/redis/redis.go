package redis

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/charlienet/gadget/broker"
	"github.com/charlienet/gadget/redis"

	redisx "github.com/redis/go-redis/v9"
)

type redisBroker struct {
	rdb      redis.Client
	mu       sync.Mutex
	subs     []*subscriber
	recvWG   sync.WaitGroup // 跟踪所有 recv goroutine 的退出
}

type subscriber struct {
	pubsub  *redisx.PubSub
	topic   string
	handler broker.Handler
	close   chan struct{}
	once    sync.Once // 保证 close(b.close) 只执行一次
	wg      *sync.WaitGroup
}

type event struct {
	topic   string
	message *broker.Message
	err     error
}

func (b *redisBroker) Publish(ctx context.Context, topic string, msg *broker.Message) error {
	return b.rdb.Publish(ctx, topic, msg).Err()
}

func (b *redisBroker) Subscribe(ctx context.Context, topic string, handler broker.Handler) (broker.Subscriber, error) {
	pubsub := b.rdb.Subscribe(ctx, topic)

	s := &subscriber{
		pubsub:  pubsub,
		topic:   topic,
		handler: handler,
		close:   make(chan struct{}),
		wg:      &b.recvWG,
	}

	b.recvWG.Add(1)

	b.mu.Lock()
	b.subs = append(b.subs, s)
	b.mu.Unlock()

	go s.recv()

	return s, nil
}

func (b *redisBroker) Name() string { return "redis" }

// Close 关闭 broker：停止所有 subscriber 的接收 goroutine 并释放 pubsub 连接。
// 最多等待 5 秒让 recv goroutine 安全退出，避免消息回调在 Close 后被触发。
func (b *redisBroker) Close(ctx context.Context) error {
	b.mu.Lock()
	subs := b.subs
	b.subs = nil
	b.mu.Unlock()

	for _, s := range subs {
		s.closeSub()
	}

	// 有界等待所有 recv goroutine 退出
	done := make(chan struct{})
	go func() {
		b.recvWG.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// 5 秒超时：recv goroutine 仍未退出，继续返回（尽力而为）
	}

	return nil
}

func (b *subscriber) Topic() string { return b.topic }

func (b *subscriber) Unsubscribe(ctx context.Context) error {
	err := b.pubsub.Unsubscribe(ctx, b.topic)
	// 取消订阅后同时停止接收 goroutine，避免 goroutine 泄漏
	b.closeSub()
	return err
}

// closeSub 幂等关闭：通知 recv goroutine 退出
func (b *subscriber) closeSub() {
	b.once.Do(func() {
		close(b.close)
	})
}

func (e *event) Ack() error               { return nil }
func (e *event) Topic() string            { return e.topic }
func (e *event) Message() *broker.Message { return e.message }
func (e *event) Error() error             { return e.err }

func (b *subscriber) recv() {
	defer b.wg.Done()
	ch := b.pubsub.Channel()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				// pubsub channel 已关闭
				return
			}
			m := broker.Message{Body: msg.Payload}
			p := event{topic: msg.Channel, message: &m}
			// handler 错误不可零感知：记 Error 日志。与 memory broker 的
			// errors.Join 聚合语义差异：redis 单消息单 handler，无聚合面。
			if err := b.handler(&p); err != nil {
				slog.Default().Error("redis broker: handler failed", "topic", p.topic, "error", err)
			}
		case <-b.close:
			// 关闭 pubsub，释放连接。Close 失败无恢复动作，
			// pubsub 已由 exit 信号停止。
			_ = b.pubsub.Close()
			return
		}
	}
}

func New(rdb redis.Client) broker.Broker {
	return &redisBroker{
		rdb: rdb,
	}
}
