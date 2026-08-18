package rabbitmq

import (
	"github.com/charlienet/gadget/broker"
	rabbitmq "github.com/rabbitmq/amqp091-go"
)

type rabbitmqBroker struct{}
type subscriber struct{}

func New() broker.Broker {
	_ = rabbitmq.Channel{}

	return &rabbitmqBroker{}
}

func (b *rabbitmqBroker) Publish(topic string, msg *broker.Message) error {
	return nil
}

func (b *rabbitmqBroker) Subscribe(topic string, handler broker.Handler) (broker.Subscriber, error) {
	return &subscriber{}, nil
}

func (b *rabbitmqBroker) Name() string { return "kafka" }

// Close 释放 broker 资源（当前实现无占用的连接资源）
func (b *rabbitmqBroker) Close() error { return nil }

func (s *subscriber) Topic() string { return "" }

func (s *subscriber) Unsubscribe() error {
	return nil
}
