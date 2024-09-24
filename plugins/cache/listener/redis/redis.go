package redis

import (
	"context"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
)

const (
	chanBufSize = 100
)

type redis_pubsub struct {
	rdb     redis.Client
	channel string
	msgChan chan string
	close   chan struct{}
}

func NewReidsListener(rdb redis.Client, channel string) cache.Listener {
	r := &redis_pubsub{
		rdb:     rdb,
		channel: channel,
		msgChan: make(chan string, chanBufSize),
		close:   make(chan struct{}),
	}

	go r.watch()

	return r
}

func (f *redis_pubsub) Initialize(opt cache.Options) {
	if len(opt.Name) > 0 {
		f.rdb = f.rdb.AddPrefix(opt.Name)
	}
}

func (r *redis_pubsub) watch() {
	sub := r.rdb.Subscribe(context.Background(), r.channel)
	c := sub.Channel()
	for {
		select {
		case msg := <-c:
			if msg != nil {
				r.msgChan <- msg.Payload
			}
		case <-r.close:
			sub.Close()
			return
		}
	}
}

func (r *redis_pubsub) Subscribe() chan string {
	return r.msgChan
}

func (r *redis_pubsub) Publish(key string) error {
	return r.rdb.Publish(context.Background(), r.channel, key).Err()
}

func (r *redis_pubsub) Close() {
	close(r.close)
}
