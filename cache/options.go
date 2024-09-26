package cache

import (
	"github.com/charlienet/gadget/broker"
	"github.com/charlienet/gadget/logger"
)

// Options represents the options for the cache.
type Options struct {
	localStore  Store
	remoteStore Store
	listener    Listener
	broker      broker.Broker
	serializer  Serializer
	Logger      logger.Logger
	TTL         int
	Name        string
}

func (o Options) init() {
	o.initActual(o.localStore)
	o.initActual(o.remoteStore)
	o.initActual(o.listener)
}

func (o Options) initActual(v any) {
	if v == nil {
		return
	}

	if i, ok := v.(interface{ Initialize(Options) }); ok {
		i.Initialize(o)
	}
}

func (o *Options) WithStore(s Store) {
	if !s.IsRemote() {
		o.localStore = s
	} else {
		o.remoteStore = s
	}
}

func (o *Options) WithListener(l Listener) {
	o.listener = l
}

// Option manipulates the Options passed.
type Option func(o *Options)

func WithName(name string) Option {
	return func(o *Options) {
		o.Name = name
	}
}

func WithMemStore() Option {
	return func(o *Options) {
		o.WithStore(newMemStore())
	}
}

func WithStore(s Store) Option {
	return func(o *Options) {
		o.WithStore(s)
	}
}

func WithListener(lis Listener) Option {
	return func(o *Options) {
		o.listener = lis
	}
}

func (o *Options) WithBroker(b broker.Broker) Option {
	return func(o *Options) {
		o.broker = b
	}
}

func WithSerializer(s Serializer) Option {
	return func(o *Options) {
		o.serializer = s
	}
}

func WithTTL(ttl int) Option {
	return func(o *Options) {
		o.TTL = ttl
	}
}

func WithLogger(l logger.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}
