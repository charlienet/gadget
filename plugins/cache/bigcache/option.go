package bigcache

import "git.charlienet.top/go/gadget/cache"

func New() cache.Option {
	return func(o *cache.Options) {
		b := NewBigCache()
		o.WithStore(b)
	}
}
