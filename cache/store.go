package cache

import "context"

// Store is the interface that wraps the cache store.
type Store interface {
	// Get gets a cached value by key.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Put stores a key-value pair into cache.
	Put(ctx context.Context, key string, v []byte, expireSecond int) error
	// Delete removes a key from cache.
	Delete(ctx context.Context, key ...string) error
	// String returns the name of the implementation.
	Name() string
	//  is remote storage
	IsRemote() bool
}
