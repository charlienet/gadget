package cache

import "context"

// Store is the interface that wraps the cache store.
//
// 实现必须并发安全：cache 包的 healthLoop 会在后台并发调用 Get/Put/Delete，
// 用户的 Get/Put/Delete 请求也会并发执行。
// Close 与 Get/Put/Delete 等数据操作须并发安全。
type Store interface {

	// Get gets a cached value by key.
	Get(ctx context.Context, key string) ([]byte, bool, error)

	// Put stores a key-value pair into cache.
	Put(ctx context.Context, key string, v []byte, expireSecond int) error

	// Delete removes a key from cache.
	Delete(ctx context.Context, key ...string) error

	// Name returns the name of the implementation.
	Name() string

	// IsRemote reports whether this store is remote (network-backed).
	IsRemote() bool
}

// PatternStore is an optional interface that stores can implement to provide
// pattern-based key deletion (e.g. "user:*").
type PatternStore interface {
	DeletePattern(ctx context.Context, pattern string) error
}

// BulkStore is an optional interface that stores can implement to provide
// optimized batch operations. If a store does not implement BulkStore,
// the cache falls back to iterating individual Get/Put calls.
type BulkStore interface {
	// GetMulti retrieves multiple keys at once. Returns a map of found keys.
	// Missing keys are not present in the returned map.
	GetMulti(ctx context.Context, keys ...string) (map[string][]byte, error)

	// SetMulti stores multiple key-value pairs atomically (best-effort).
	SetMulti(ctx context.Context, items map[string][]byte, expireSecond int) error
}
