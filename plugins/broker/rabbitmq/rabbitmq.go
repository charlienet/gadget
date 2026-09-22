package rabbitmq

import (
	"context"
	"errors"

	"github.com/charlienet/gadget/broker"
)

type rabbitmqBroker struct{}
type subscriber struct{}

func New() broker.Broker {
	return &rabbitmqBroker{}
}

func (b *rabbitmqBroker) Publish(ctx context.Context, topic string, msg *broker.Message) error {
	return errors.New("rabbitmq: broker not implemented")
}

func (b *rabbitmqBroker) Subscribe(ctx context.Context, topic string, handler broker.Handler) (broker.Subscriber, error) {
	return nil, errors.New("rabbitmq: broker not implemented")
}

func (b *rabbitmqBroker) Name() string { return "rabbitmq" }

// Close 释放 broker 资源（当前实现无占用的连接资源）
func (b *rabbitmqBroker) Close(ctx context.Context) error {
	return errors.New("rabbitmq: broker not implemented")
}

func (s *subscriber) Topic() string { return "" }

func (s *subscriber) Unsubscribe(ctx context.Context) error {
	return errors.New("rabbitmq: broker not implemented")
}
