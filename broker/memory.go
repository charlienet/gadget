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
	exit    chan bool
	handler Handler
	id      string
	topic   string
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
			// p.err = err
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
		size := len(m.Subscribers) - 1
		newSubscribers := make([]*memorySubscriber, 0, size)
		for _, s := range m.Subscribers[topic] {
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

func (m *memorySubscriber) Topic() string {
	return m.topic
}

func (m *memorySubscriber) Unsubscribe() error {
	m.exit <- true
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
