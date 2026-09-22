package broker

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
)

// subIDCounter 用于生成进程内唯一的订阅 id
var subIDCounter atomic.Uint64

var _ Broker = &memoryBroker{}

type memoryBroker struct {
	subscribers map[string][]*memorySubscriber
	mu          sync.RWMutex
}

type memorySubscriber struct {
	exit      chan struct{}
	handler   Handler
	id        string
	topic     string
	closeOnce sync.Once // 保证 exit channel 只关闭一次
}

type memoryEvent struct {
	message any
	topic   string
	err     error
}

// Publish 将消息同步派发给 topic 下的全部订阅者：
// 所有 handler 在 Publish 返回前同步执行完毕（异步化属于 broker v0.2.0 接口重设计范畴，当前版本不提供）。
// 每个订阅者收到独立的 Event 拷贝（Body 相同，Ack 与错误等元数据互相隔离）。
// 返回值聚合全部 handler 错误（errors.Join），单个 handler 报错不影响对其余订阅者的投递。
func (m *memoryBroker) Publish(ctx context.Context, topic string, msg *Message) error {
	m.mu.RLock()
	subs, ok := m.subscribers[topic]
	m.mu.RUnlock()

	if !ok {
		return nil
	}

	var errs []error
	for _, sub := range subs {
		// 每个订阅者派发独立的 Event 拷贝，避免共享 Ack/错误状态互相干扰
		p := &memoryEvent{message: msg, topic: topic}
		if err := sub.handler(p); err != nil {
			// 记录 handler 错误，供事件订阅方通过 Error() 读取
			p.err = err
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (m *memoryBroker) Subscribe(ctx context.Context, topic string, handler Handler) (Subscriber, error) {
	sub := &memorySubscriber{
		exit:    make(chan struct{}),
		id:      "sub-" + strconv.FormatUint(subIDCounter.Add(1), 10),
		topic:   topic,
		handler: handler,
	}

	m.mu.Lock()
	m.subscribers[topic] = append(m.subscribers[topic], sub)
	m.mu.Unlock()

	go func() {
		<-sub.exit
		m.mu.Lock()
		subs := m.subscribers[topic]
		newSubscribers := make([]*memorySubscriber, 0, len(subs))
		for _, s := range subs {
			if s.id != sub.id {
				newSubscribers = append(newSubscribers, s)
			}
		}
		m.subscribers[topic] = newSubscribers
		m.mu.Unlock()
	}()

	return sub, nil
}

func (m *memoryBroker) Name() string { return "memory" }

// Close 关闭 broker：通知所有 subscriber 退出并清空订阅
func (m *memoryBroker) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, subs := range m.subscribers {
		for _, sub := range subs {
			sub.closeOnce.Do(func() {
				close(sub.exit)
			})
		}
	}
	m.subscribers = make(map[string][]*memorySubscriber)

	return nil
}

func (m *memorySubscriber) Topic() string {
	return m.topic
}

func (m *memorySubscriber) Unsubscribe(ctx context.Context) error {
	// 通过关闭 exit 通知 goroutine 退出（幂等，避免在无缓冲 channel 上阻塞发送）
	m.closeOnce.Do(func() {
		close(m.exit)
	})
	return nil
}

func (m *memoryEvent) Message() *Message {
	v, _ := m.message.(*Message)
	return v
}

func (m *memoryEvent) Topic() string { return m.topic }
func (m *memoryEvent) Ack() error    { return nil }
func (m *memoryEvent) Error() error  { return m.err }

func NewMemoryBroker() Broker {
	return &memoryBroker{
		subscribers: make(map[string][]*memorySubscriber),
	}
}
