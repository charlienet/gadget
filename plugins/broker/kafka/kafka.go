package kafka

import (
	"context"
	"errors"

	"github.com/charlienet/gadget/broker"
)

type kafkaBroker struct{}
type subscriber struct{}

func New() broker.Broker {
	return &kafkaBroker{}
}

func (b *kafkaBroker) Publish(ctx context.Context, topic string, msg *broker.Message) error {
	return errors.New("kafka: broker not implemented")
}

func (b *kafkaBroker) Subscribe(ctx context.Context, topic string, handler broker.Handler) (broker.Subscriber, error) {
	return nil, errors.New("kafka: broker not implemented")
}

func (b *kafkaBroker) Name() string { return "kafka" }

// Close 释放 broker 资源（当前实现无占用的连接资源）
func (b *kafkaBroker) Close(ctx context.Context) error {
	return errors.New("kafka: broker not implemented")
}

func (s *subscriber) Topic() string { return "" }

func (s *subscriber) Unsubscribe(ctx context.Context) error {
	return errors.New("kafka: broker not implemented")
}
