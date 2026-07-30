package memcached

import "git.charlienet.top/go/gadget/cache"

func New(addrs ...string) cache.Option {
	return func(o *cache.Options) {
		o.WithStore(new(addrs...))
	}
}
