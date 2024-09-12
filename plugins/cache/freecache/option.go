package freecache

import "github.com/charlienet/gadget/cache"

func New(size int) cache.Option {
	return func(o *cache.Options) {
		s := new(size)
		o.AddStore(s)
	}
}
