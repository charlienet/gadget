package broker_test

import (
	"errors"
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

		_ = b.Publish(topic, msg)
	}

	_ = sub.Unsubscribe()
	assert.Equal(t, count, received)
}

// TestMemoryBrokerPublishEventIsolation 回归缺陷 2：
// 多订阅者派发时每个 handler 必须收到独立的 Event 拷贝，
// 订阅者 A 的 Ack/错误状态不得串扰到订阅者 B 的 Event。
func TestMemoryBrokerPublishEventIsolation(t *testing.T) {
	b := broker.NewMemoryBroker()
	defer func() { assert.NoError(t, b.Close()) }()

	errA := errors.New("handler a failed")
	var events [2]broker.Event

	// A 先订阅且先返回错误；旧实现共享同一 event 时，B 的 Error() 会读到 A 的错误
	sa, err := b.Subscribe("isolation", func(p broker.Event) error {
		events[0] = p
		_ = p.Ack()
		return errA
	})
	assert.NoError(t, err)

	sb, err := b.Subscribe("isolation", func(p broker.Event) error {
		events[1] = p
		// B 执行时其 Event 的错误元数据应保持独立（不被 A 的失败污染）
		assert.NoError(t, p.Error(), "B 的 event 错误状态被订阅者 A 污染")
		return nil
	})
	assert.NoError(t, err)

	err = b.Publish("isolation", &broker.Message{Body: "hello"})
	assert.ErrorIs(t, err, errA)

	assert.True(t, events[0] != events[1], "订阅者 A/B 收到了同一个 Event 实例")
	assert.ErrorIs(t, events[0].Error(), errA)
	assert.NoError(t, events[1].Error())

	assert.NoError(t, sa.Unsubscribe())
	assert.NoError(t, sb.Unsubscribe())
}

// TestMemoryBrokerPublishAggregatesHandlerErrors 回归缺陷 3：
// Publish 必须用 errors.Join 聚合全部 handler 错误（而非丢弃或只留最后一个），
// 且单个 handler 报错不影响对其余订阅者的投递。
func TestMemoryBrokerPublishAggregatesHandlerErrors(t *testing.T) {
	b := broker.NewMemoryBroker()
	defer func() { assert.NoError(t, b.Close()) }()

	errA := errors.New("handler a failed")
	errB := errors.New("handler b failed")
	errC := errors.New("handler c failed")

	var delivered int32
	handlers := []broker.Handler{
		func(broker.Event) error { atomic.AddInt32(&delivered, 1); return errA },
		func(broker.Event) error { atomic.AddInt32(&delivered, 1); return errB },
		func(broker.Event) error { atomic.AddInt32(&delivered, 1); return nil },
		func(broker.Event) error { atomic.AddInt32(&delivered, 1); return errC },
	}
	for i, h := range handlers {
		_, err := b.Subscribe("aggregate", h)
		assert.NoError(t, err, "subscribe handler %d", i)
	}

	err := b.Publish("aggregate", &broker.Message{Body: "hello"})
	assert.ErrorIs(t, err, errA)
	assert.ErrorIs(t, err, errB)
	assert.ErrorIs(t, err, errC)
	assert.Contains(t, err.Error(), "handler a failed")
	assert.Contains(t, err.Error(), "handler b failed")
	assert.Contains(t, err.Error(), "handler c failed")

	// 全部订阅者（含报错 handler 的后续订阅者）均被投递
	assert.Equal(t, int32(4), atomic.LoadInt32(&delivered))
}
