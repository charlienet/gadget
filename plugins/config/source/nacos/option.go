package nacos

import "github.com/nacos-group/nacos-sdk-go/v2/common/constant"

type Option func(*Options)

type Options struct {
	address   []string
	namespace string
	group     string
}

func WithAddress(addrs []string) Option {
	return func(o *Options) {
		o.address = addrs
	}
}

func WithNamespace(namespace string) Option {
	return func(o *Options) {
		o.namespace = namespace
	}
}

func WithGroup(g string) Option {
	return func(o *Options) {
		o.group = g
	}
}

func WithClientConfig(cc constant.ClientConfig) Option {
	return func(o *Options) {
	}
}

func WithWatch() Option {
	return func(o *Options) {}
}
