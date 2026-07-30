// Package stream provides a cache.Listener implementation backed by Redis
// Streams with Consumer Groups. Unlike the PubSub-based listener, this
// provides at-least-once delivery semantics: failed messages remain in the
// stream as pending entries and are re-delivered on restart.
package stream

import (
	"context"
	"time"

	"git.charlienet.top/go/gadget/cache"
	"git.charlienet.top/go/gadget/redis"
	goredis "github.com/redis/go-redis/v9"
)

const (
	defaultStreamName  = "cache:invalidate"
	defaultGroupName   = "cache-group"
	defaultConsumerID  = "cache-consumer"
	readBlockTimeout   = 2 * time.Second
	chanBufSize        = 100
	maxMessagesPerRead = 10
)

type Option func(*streamListener)

// WithStreamName sets the Redis Stream key used for invalidation messages.
func WithStreamName(name string) Option {
	return func(s *streamListener) {
		s.stream = name
	}
}

// WithConsumerGroup sets the consumer group name.
func WithConsumerGroup(name string) Option {
	return func(s *streamListener) {
		s.group = name
	}
}

// WithConsumerID sets the consumer ID within the group.
func WithConsumerID(id string) Option {
	return func(s *streamListener) {
		s.consumer = id
	}
}

type streamListener struct {
	rdb      redis.Client
	stream   string
	group    string
	consumer string
	msgChan  chan string
	close    chan struct{}
}

// NewStreamListener creates a Listener that uses Redis Streams for reliable
// cache invalidation. The consumer group is created lazily (if it doesn't
// already exist). Messages are acknowledged after being delivered to the
// Subscribe channel.
func NewStreamListener(rdb redis.Client, opts ...Option) cache.Listener {
	s := &streamListener{
		rdb:      rdb,
		stream:   defaultStreamName,
		group:    defaultGroupName,
		consumer: defaultConsumerID,
		msgChan:  make(chan string, chanBufSize),
		close:    make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}

	// Create consumer group (no-op if already exists)
	_ = s.rdb.XGroupCreate(context.Background(), s.stream, s.group, "0").Err()

	go s.watch()

	return s
}

func (s *streamListener) Initialize(opt cache.Options) {
	if len(opt.Name) > 0 {
		s.stream = opt.Name + ":" + s.stream
	}
}

func (s *streamListener) Subscribe() chan string {
	return s.msgChan
}

func (s *streamListener) Publish(key string) error {
	return s.rdb.XAdd(context.Background(), &goredis.XAddArgs{
		Stream: s.stream,
		Values: map[string]interface{}{"key": key},
	}).Err()
}

func (s *streamListener) Close() {
	close(s.close)
}

func (s *streamListener) watch() {
	for {
		select {
		case <-s.close:
			return
		default:
		}

		result, err := s.rdb.XReadGroup(context.Background(), &goredis.XReadGroupArgs{
			Group:    s.group,
			Consumer: s.consumer,
			Streams:  []string{s.stream, ">"},
			Count:    int64(maxMessagesPerRead),
			Block:    readBlockTimeout,
		}).Result()

		if err != nil {
			select {
			case <-s.close:
				return
			default:
				continue
			}
		}

		for _, stream := range result {
			for _, msg := range stream.Messages {
				key, ok := msg.Values["key"].(string)
				if !ok || key == "" {
					continue
				}

				select {
				case s.msgChan <- key:
					// Acknowledge after delivering to channel
					s.rdb.XAck(context.Background(), s.stream, s.group, msg.ID)
				case <-s.close:
					return
				}
			}
		}
	}
}
