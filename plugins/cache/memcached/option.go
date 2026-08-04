package memcached

import "github.com/charlienet/gadget/cache"

func New(addrs ...string) cache.Option {
	return func(o *cache.Options) {
		o.WithStore(new(addrs...))
	}
}
