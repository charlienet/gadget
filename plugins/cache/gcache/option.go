package gcache

import "github.com/charlienet/gadget/cache"

func New(size int) cache.Option {
	return func(o *cache.Options) {
		o.WithStore(newGcache(size))
	}
}
