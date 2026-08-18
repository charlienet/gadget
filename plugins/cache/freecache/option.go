package freecache

import (
	"fmt"

	"github.com/charlienet/gadget/cache"
)

// New returns a cache.Option that installs a freecache-backed local store
// with the given capacity, in bytes. The underlying library silently promotes
// any size below 512KB (512*1024 bytes) up to 512KB, so the effective memory
// allocation may exceed the requested value. It also enforces hard entry
// limits: keys must be <= 65535 bytes (ErrLargeKey) and values must be
// <= 1/1024 of the cache size (ErrLargeEntry). It returns an error when
// size <= 0.
func New(size int) (cache.Option, error) {
	if size <= 0 {
		return nil, fmt.Errorf("freecache: size must be > 0, got %d", size)
	}

	return func(o *cache.Options) {
		s := new(size)
		o.WithStore(s)
	}, nil
}
