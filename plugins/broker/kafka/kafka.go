package kafka

import (
	"github.com/charlienet/gadget/broker"
	_ "github.com/segmentio/kafka-go"
)

type kafkaBroker struct{}
type subscriber struct{}

func New() broker.Broker {
	return &kafkaBroker{}
}

func (b *kafkaBroker) Publish(topic string, msg *broker.Message) error {
	return nil
}

func (b *kafkaBroker) Subscribe(topic string, handler broker.Handler) (broker.Subscriber, error) {
	return &subscriber{}, nil
}

func (b *kafkaBroker) Name() string { return "kafka" }

func (s *subscriber) Topic() string { return "" }

func (s *subscriber) Unsubscribe() error {
	return nil
}
