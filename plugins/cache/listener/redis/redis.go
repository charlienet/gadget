package redis

import (
	"context"

	"github.com/charlienet/gadget/redis"
)

type redis_pubsub struct {
	rdb     redis.Client
	channel string
	close   chan bool
}

func newReidsStore(rdb redis.Client, channel string) *redis_pubsub {
	r := &redis_pubsub{
		rdb:     rdb,
		channel: channel,
		close:   make(chan bool),
	}

	go r.listen()

	return r
}

func (r *redis_pubsub) listen() {
	pubsub := r.rdb.Subscribe(context.Background(), r.channel)
	c := pubsub.Channel()
	for {
		select {
		case msg := <-c:
			println("收到消息:", msg.Payload)
		case <-r.close:
			pubsub.Close()
			println("关闭")
			return
		}
	}
}

func (r *redis_pubsub) Publish(msg any) {
	r.rdb.Publish(context.Background(), r.channel, msg)
}

func (r *redis_pubsub) Close() {
	close(r.close)
}
