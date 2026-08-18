package gcache

import "github.com/charlienet/gadget/cache"

// New returns a cache.Option that installs an LRU local cache with the given
// capacity, in number of entries (not bytes). It returns an error when
// size <= 0, because the underlying library would otherwise panic in Build().
func New(size int) (cache.Option, error) {
	s, err := newGcache(size)
	if err != nil {
		return nil, err
	}

	return func(o *cache.Options) {
		o.WithStore(s)
	}, nil
}
