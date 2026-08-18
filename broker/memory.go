package broker

import (
	"sync"

	"github.com/google/uuid"
)

var _ Broker = &memoryBroker{}

type memoryBroker struct {
	Subscribers map[string][]*memorySubscriber
	sync.RWMutex
}

type memorySubscriber struct {
	exit      chan bool
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

func (m *memoryBroker) Publish(topic string, msg *Message) error {
	m.RLock()
	subs, ok := m.Subscribers[topic]
	m.RUnlock()

	if !ok {
		return nil
	}

	p := &memoryEvent{message: msg, topic: topic}
	for _, sub := range subs {
		if err := sub.handler(p); err != nil {
			// 记录 handler 错误，供事件订阅方通过 Error() 读取
			p.err = err
		}
	}

	return nil
}

func (m *memoryBroker) Subscribe(topic string, handler Handler) (Subscriber, error) {
	sub := &memorySubscriber{
		exit:    make(chan bool),
		id:      uuid.New().String(),
		topic:   topic,
		handler: handler,
	}

	m.Lock()
	m.Subscribers[topic] = append(m.Subscribers[topic], sub)
	m.Unlock()

	go func() {
		<-sub.exit
		m.Lock()
		subs := m.Subscribers[topic]
		newSubscribers := make([]*memorySubscriber, 0, len(subs))
		for _, s := range subs {
			if s.id != sub.id {
				newSubscribers = append(newSubscribers, s)
			}
		}
		m.Subscribers[topic] = newSubscribers
		m.Unlock()
	}()

	return sub, nil
}

func (m *memoryBroker) Name() string { return "memory" }

// Close 关闭 broker：通知所有 subscriber 退出并清空订阅
func (m *memoryBroker) Close() error {
	m.Lock()
	defer m.Unlock()

	for _, subs := range m.Subscribers {
		for _, sub := range subs {
			sub.closeOnce.Do(func() {
				close(sub.exit)
			})
		}
	}
	m.Subscribers = make(map[string][]*memorySubscriber)

	return nil
}

func (m *memorySubscriber) Topic() string {
	return m.topic
}

func (m *memorySubscriber) Unsubscribe() error {
	// 通过关闭 exit 通知 goroutine 退出（幂等，避免在无缓冲 channel 上阻塞发送）
	m.closeOnce.Do(func() {
		close(m.exit)
	})
	return nil
}

func (m *memoryEvent) Message() *Message {
	switch v := m.message.(type) {
	case *Message:
		return v
	}

	return nil
}

func (m *memoryEvent) Topic() string { return m.topic }
func (m *memoryEvent) Ack() error    { return nil }
func (m *memoryEvent) Error() error  { return m.err }

func NewMemoryBroker() Broker {
	return &memoryBroker{
		Subscribers: make(map[string][]*memorySubscriber),
	}
}
