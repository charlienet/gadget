package redis

import (
	"context"
	"sync"

	"github.com/charlienet/gadget/broker"
	"github.com/charlienet/gadget/redis"

	redisx "github.com/redis/go-redis/v9"
)

type redisBroker struct {
	rdb  redis.Client
	mu   sync.Mutex
	subs []*subscriber
}

type subscriber struct {
	pubsub  *redisx.PubSub
	topic   string
	handler broker.Handler
	close   chan struct{}
	once    sync.Once // 保证 close(b.close) 只执行一次
}

type event struct {
	topic   string
	message *broker.Message
	err     error
}

func (b *redisBroker) Publish(topic string, msg *broker.Message) error {
	return b.rdb.Publish(context.Background(), topic, msg).Err()
}

func (b *redisBroker) Subscribe(topic string, handler broker.Handler) (broker.Subscriber, error) {
	pubsub := b.rdb.Subscribe(context.Background(), topic)

	s := &subscriber{
		pubsub:  pubsub,
		topic:   topic,
		handler: handler,
		close:   make(chan struct{}),
	}

	b.mu.Lock()
	b.subs = append(b.subs, s)
	b.mu.Unlock()

	go s.recv()

	return s, nil
}

func (b *redisBroker) Name() string { return "redis" }

// Close 关闭 broker：停止所有 subscriber 的接收 goroutine 并释放 pubsub 连接。
func (b *redisBroker) Close() error {
	b.mu.Lock()
	subs := b.subs
	b.subs = nil
	b.mu.Unlock()

	for _, s := range subs {
		s.closeSub()
	}

	return nil
}

func (b *subscriber) Topic() string { return b.topic }

func (b *subscriber) Unsubscribe() error {
	err := b.pubsub.Unsubscribe(context.Background(), b.topic)
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
			_ = b.handler(&p)
		case <-b.close:
			// 关闭 pubsub，释放连接
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
