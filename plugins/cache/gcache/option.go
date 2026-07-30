package gcache

import "git.charlienet.top/go/gadget/cache"

func New(size int) cache.Option {
	return func(o *cache.Options) {
		o.WithStore(newGcache(size))
	}
}
