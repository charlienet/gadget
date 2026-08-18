package broker

// Asynchronous message broker
type Broker interface {
	Publish(topic string, m *Message) error
	Subscribe(topic string, h Handler) (Subscriber, error)
	Name() string
	// Close 释放 broker 占用的资源（连接、订阅 goroutine 等）
	Close() error
}

// Subscriber is a convenience return type for the Subscribe method.
type Subscriber interface {
	Topic() string
	Unsubscribe() error
}

// message send/received from the broker.
type Message struct {
	Body string
}

// Event is given to a subscription handler for processing.
type Event interface {
	Topic() string
	Message() *Message
	Ack() error
	Error() error
}

type Handler func(Event) error
