package broker

import "context"

// Asynchronous message broker
type Broker interface {
	Publish(ctx context.Context, topic string, m *Message) error
	Subscribe(ctx context.Context, topic string, h Handler) (Subscriber, error)
	Name() string
	Close(ctx context.Context) error
}

// Subscriber is a convenience return type for the Subscribe method.
type Subscriber interface {
	Topic() string
	Unsubscribe(ctx context.Context) error
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
