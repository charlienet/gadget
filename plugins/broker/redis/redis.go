package redis

import (
	"context"

	"github.com/charlienet/gadget/broker"
	"github.com/charlienet/gadget/redis"

	redisx "github.com/redis/go-redis/v9"
)

type redisBroker struct {
	rdb redis.Client
}

type subscriber struct {
	pubsub  *redisx.PubSub
	topic   string
	handler broker.Handler
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

	s := subscriber{
		pubsub:  pubsub,
		topic:   topic,
		handler: handler,
	}

	go s.recv()

	return &s, nil
}

func (b *redisBroker) Name() string { return "redis" }

func (b *subscriber) Topic() string { return b.topic }

func (b *subscriber) Unsubscribe() error {
	return b.pubsub.Unsubscribe(context.Background(), b.topic)
}

func (e *event) Ack() error               { return nil }
func (e *event) Topic() string            { return e.topic }
func (e *event) Message() *broker.Message { return e.message }
func (e *event) Error() error             { return e.err }

func (b *subscriber) recv() {
	ch := b.pubsub.Channel()
	for msg := range ch {
		m := broker.Message{Body: msg.Payload}
		p := event{topic: msg.Channel, message: &m}
		b.handler(&p)
	}
}

func New(rdb redis.Client) broker.Broker {
	return &redisBroker{
		rdb: rdb,
	}
}
