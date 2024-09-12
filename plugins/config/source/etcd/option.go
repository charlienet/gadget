package etcd

import "time"

type Option func(*Options)

type Options struct {
	Endpoints  []string
	DialTimout time.Duration
	username   string
	password   string
}

func WithAddr(addr string) Option {
	return func(o *Options) {
		o.Endpoints = []string{addr}
	}
}

func WithAddrs(addrs []string) Option {
	return func(o *Options) {
		o.Endpoints = addrs
	}
}

func WithUserName(username, password string) Option {
	return func(o *Options) {
		o.username = username
		o.password = password
	}
}
