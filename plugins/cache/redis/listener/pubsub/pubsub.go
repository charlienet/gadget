// Package pubsub provides a cache.Listener implementation backed by Redis
// PubSub with automatic reconnection on connection failure.
package pubsub

import (
	"context"
	"time"

	"git.charlienet.top/go/gadget/cache"
	"git.charlienet.top/go/gadget/redis"
)

const (
	chanBufSize     = 100
	healthCheckIntv = 15 * time.Second // ping interval
	reconnectDelay  = time.Second      // delay between reconnection attempts
)

type pubSubListener struct {
	rdb     redis.Client
	channel string
	msgChan chan string
	close   chan struct{}
}

// NewListener creates a cache.Listener backed by Redis PubSub. It includes
// automatic health checks and reconnection when the connection drops.
func NewListener(rdb redis.Client, channel string) cache.Listener {
	r := &pubSubListener{
		rdb:     rdb,
		channel: channel,
		msgChan: make(chan string, chanBufSize),
		close:   make(chan struct{}),
	}

	go r.watch()

	return r
}

func (f *pubSubListener) Initialize(opt cache.Options) {
	if len(opt.Name) > 0 {
		f.rdb = f.rdb.AddPrefix(opt.Name)
	}
}

func (r *pubSubListener) watch() {
	sub := r.rdb.Subscribe(context.Background(), r.channel)
	c := sub.Channel()
	healthTicker := time.NewTicker(healthCheckIntv)
	defer healthTicker.Stop()

	for {
		select {
		case msg := <-c:
			if msg != nil {
				r.msgChan <- msg.Payload
			}
		case <-healthTicker.C:
			// Periodic health check: if ping fails, reconnect
			if err := sub.Ping(context.Background()); err != nil {
				sub.Close()
				// Reconnect with a small delay
				select {
				case <-r.close:
					return
				case <-time.After(reconnectDelay):
				}
				sub = r.rdb.Subscribe(context.Background(), r.channel)
				c = sub.Channel()
			}
		case <-r.close:
			sub.Close()
			return
		}
	}
}

func (r *pubSubListener) Subscribe() chan string {
	return r.msgChan
}

func (r *pubSubListener) Publish(key string) error {
	return r.rdb.Publish(context.Background(), r.channel, key).Err()
}

func (r *pubSubListener) Close() {
	close(r.close)
}
