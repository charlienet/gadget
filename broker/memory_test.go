package broker_test

import (
	"sync/atomic"
	"testing"

	"github.com/charlienet/gadget/broker"
	"github.com/stretchr/testify/assert"
)

func TestMemoryBroker(t *testing.T) {
	b := broker.NewMemoryBroker()

	topic := "test"
	var count int32 = 10
	var received int32 = 0

	fn := func(p broker.Event) error {
		atomic.AddInt32(&received, 1)
		return nil
	}

	sub, err := b.Subscribe(topic, fn)
	assert.Nil(t, err)

	for range count {
		msg := &broker.Message{
			Body: "hello",
		}

		b.Publish(topic, msg)
	}

	sub.Unsubscribe()
	assert.Equal(t, count, received)
}
