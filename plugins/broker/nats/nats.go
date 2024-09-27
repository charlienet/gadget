package nats

import (
	"errors"

	"github.com/charlienet/gadget/broker"
	nats "github.com/nats-io/nats.go"
)

type natsBroker struct {
	conn  *nats.Conn
	nopts nats.Options
}

type subscriber struct {
	s *nats.Subscription
}

func New() broker.Broker {
	b := natsBroker{nopts: nats.GetDefaultOptions()}

	c, err := b.nopts.Connect()
	_ = err

	b.conn = c

	return &b
}

func (n *natsBroker) Publish(topic string, msg *broker.Message) error {
	if n.conn == nil {
		return errors.New("not connected")
	}

	return n.conn.Publish(topic, []byte(msg.Body))
}

func (n *natsBroker) Subscribe(topic string, handler broker.Handler) (broker.Subscriber, error) {
	if n.conn == nil {
		return nil, errors.New("not connected")
	}

	fn := func(msg *nats.Msg) {
	}
	_ = fn

	var sub *nats.Subscription
	var err error
	sub, err = n.conn.Subscribe(topic, fn)
	if err != nil {
		return nil, err
	}

	return &subscriber{s: sub}, nil
}

func (b *natsBroker) Name() string { return "nats" }

func (s *subscriber) Topic() string      { return s.s.Subject }
func (s *subscriber) Unsubscribe() error { return s.s.Unsubscribe() }
