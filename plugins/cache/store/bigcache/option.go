package bigcache

import "github.com/charlienet/gadget/cache"

func New() cache.Option {
	return func(o *cache.Options) {
		b := NewBigCache()
		o.WithStore(b)
	}
}
