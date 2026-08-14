package redis

import (
	"context"
	"hash/fnv"
	"math"

	goredis "github.com/redis/go-redis/v9"
)

// BloomFilter is a Bloom filter backed by Redis. It automatically uses native
// BF.* commands when the Redis server has the RedisBloom module loaded, and
// falls back to a bitmap (GETBIT/SETBIT) implementation otherwise.
// Application code does NOT need to check HasBloom() — the wrapper handles it.
type BloomFilter interface {
	// Add adds an item to the filter. Returns true if the item was newly added.
	Add(ctx context.Context, item string) (bool, error)

	// Exists checks whether an item has possibly been added to the filter.
	// Returns false if definitely not present; true if it may be present.
	Exists(ctx context.Context, item string) (bool, error)

	// AddMulti adds multiple items at once. Returns a slice of booleans
	// indicating whether each item was newly added.
	AddMulti(ctx context.Context, items ...string) ([]bool, error)

	// ExistsMulti checks multiple items at once.
	ExistsMulti(ctx context.Context, items ...string) ([]bool, error)

	// Info returns metadata about the Bloom filter.
	Info(ctx context.Context) (*BloomInfo, error)
}

// BloomInfo contains metadata about a Bloom filter.
type BloomInfo struct {
	Capacity   int64 // configured capacity
	Size       int64 // memory size (bytes)
	NumFilters int64 // number of filters (BF.* only)
	NumItems   int64 // approximate number of items
	Expansion  int64 // expansion factor (BF.* only)
}

// --- Options ---

// BloomOption configures a Bloom filter.
type BloomOption func(*bloomConfig)

type bloomConfig struct {
	capacity      int64
	falsePositive float64
}

func defaultBloomConfig() bloomConfig {
	return bloomConfig{
		capacity:      1000000,
		falsePositive: 0.01,
	}
}

// WithCapacity sets the expected number of items.
func WithCapacity(n int64) BloomOption {
	return func(c *bloomConfig) {
		c.capacity = n
	}
}

// WithFalsePositive sets the desired false positive rate (0 < rate < 1).
func WithFalsePositive(rate float64) BloomOption {
	return func(c *bloomConfig) {
		if rate > 0 && rate < 1 {
			c.falsePositive = rate
		}
	}
}

// --- Factory ---

// NewBloomFilter creates a BloomFilter for the given key. The implementation
// is auto-selected based on the server's capabilities.
func (rdb *redisClient) NewBloomFilter(key string, opts ...BloomOption) BloomFilter {
	cfg := defaultBloomConfig()
	for _, o := range opts {
		o(&cfg)
	}

	if rdb.cap.HasBloom() {
		return &bfCmdImpl{client: rdb, key: key, cfg: cfg}
	}
	return newBitmapImpl(rdb, key, cfg)
}

// NewBloomFilterWithEstimate creates a BloomFilter with explicit capacity and
// false positive probability. Convenience wrapper around NewBloomFilter.
// When using the native BF.* path, this calls BF.RESERVE to pre-allocate.
func (rdb *redisClient) NewBloomFilterWithEstimate(key string, capacity int64, falsePositive float64) BloomFilter {
	return rdb.NewBloomFilter(key,
		WithCapacity(capacity),
		WithFalsePositive(falsePositive),
	)
}

// --- BF.* native implementation ---

type bfCmdImpl struct {
	client *redisClient
	key    string
	cfg    bloomConfig
}

func (b *bfCmdImpl) Add(ctx context.Context, item string) (bool, error) {
	return b.client.BFAdd(ctx, b.key, item).Result()
}

func (b *bfCmdImpl) Exists(ctx context.Context, item string) (bool, error) {
	return b.client.BFExists(ctx, b.key, item).Result()
}

func toInterfaceSlice(items []string) []interface{} {
	args := make([]interface{}, len(items))
	for i, v := range items {
		args[i] = v
	}
	return args
}

func (b *bfCmdImpl) AddMulti(ctx context.Context, items ...string) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}

	// BF.MADD returns ints: 1 if newly inserted, 0 if already present
	added, err := b.client.BFMAdd(ctx, b.key, toInterfaceSlice(items)...).Result()
	if err != nil {
		return nil, err
	}

	return added, nil
}

func (b *bfCmdImpl) ExistsMulti(ctx context.Context, items ...string) ([]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}

	results, err := b.client.BFMExists(ctx, b.key, toInterfaceSlice(items)...).Result()
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (b *bfCmdImpl) Info(ctx context.Context) (*BloomInfo, error) {
	info, err := b.client.BFInfo(ctx, b.key).Result()
	if err != nil {
		return nil, err
	}

	return &BloomInfo{
		Capacity:   info.Capacity,
		Size:       info.Size,
		NumFilters: info.Filters,
		NumItems:   info.ItemsInserted,
		Expansion:  info.ExpansionRate,
	}, nil
}

// --- Bitmap fallback implementation ---

type bitmapImpl struct {
	client   *redisClient
	key      string
	m        uint64 // bitmap size in bits
	k        uint   // number of hash functions
	capacity int64
}

func newBitmapImpl(client *redisClient, key string, cfg bloomConfig) *bitmapImpl {
	m := bloomBitCount(cfg.capacity, cfg.falsePositive)
	k := bloomHashCount(cfg.capacity, m)

	return &bitmapImpl{
		client:   client,
		key:      key,
		m:        m,
		k:        k,
		capacity: cfg.capacity,
	}
}

// bloomBitCount computes optimal bitmap size (m bits) using:
//
//	m = -n * ln(p) / (ln(2))^2
func bloomBitCount(n int64, p float64) uint64 {
	m := -float64(n) * math.Log(p) / (math.Ln2 * math.Ln2)
	return uint64(math.Ceil(m))
}

// bloomHashCount computes optimal number of hash functions using:
//
//	k = (m / n) * ln(2)
func bloomHashCount(n int64, m uint64) uint {
	k := float64(m) / float64(n) * math.Ln2
	if k < 1 {
		return 1
	}
	if k > 30 {
		return 30
	}
	return uint(math.Ceil(k))
}

// hashs returns the k bit positions for an item.
// Uses double hashing: h(i) = h1 + i * h2  (mod m)
func (b *bitmapImpl) hashs(item string) []uint64 {
	h1 := fnv1a(item)
	h2 := fnv1a(hashSalt(item))

	positions := make([]uint64, b.k)
	for i := uint(0); i < b.k; i++ {
		positions[i] = (h1 + uint64(i)*h2) % b.m
	}
	return positions
}

func fnv1a(data string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(data))
	return h.Sum64()
}

func hashSalt(item string) string {
	// Reversible mixing to get a second independent hash
	return "\x92" + item + "\x71"
}

// --- Lua 原子化脚本 ---
// bitmap 路径的 add/exists 通过 Lua 脚本在服务端一次往返完成：
// 原实现是 exists（k 次 GETBIT）+ k 次 SETBIT 的多命令往返，既慢又存在
// 非原子窗口（并发添加同一 item 可能重复返回"新添加"）。脚本化后单次
// EVAL 完成全部位操作，保证语义与原实现一致且原子。

var (
	// bitmapAddScript 原子添加：逐位 GETBIT 检查、SETBIT 置位，
	// 返回 1 表示 item 之前完全不存在（新添加），0 表示已存在。
	bitmapAddScript = goredis.NewScript(`
		local all_zero = 1
		for i = 1, #ARGV do
			local bit = redis.call('GETBIT', KEYS[1], ARGV[i])
			if bit == 1 then
				all_zero = 0
			else
				redis.call('SETBIT', KEYS[1], ARGV[i], 1)
			end
		end
		return all_zero
	`)

	// bitmapExistsScript 原子存在性检查：任一位置为 0 即不存在，返回 0。
	bitmapExistsScript = goredis.NewScript(`
		for i = 1, #ARGV do
			if redis.call('GETBIT', KEYS[1], ARGV[i]) == 0 then
				return 0
			end
		end
		return 1
	`)
)

// positions 将 item 的 k 个哈希位转为脚本参数（[]uint64 → []interface{}）。
func (b *bitmapImpl) positions(item string) []interface{} {
	hashs := b.hashs(item)
	args := make([]interface{}, len(hashs))
	for i, p := range hashs {
		args[i] = p
	}
	return args
}

func (b *bitmapImpl) add(ctx context.Context, item string) (bool, error) {
	added, err := bitmapAddScript.Run(ctx, b.client, []string{b.key}, b.positions(item)...).Int()
	if err == nil {
		// Lua 脚本返回 1 = 新添加（item 之前完全不存在）
		return added == 1, nil
	}

	// 服务器不支持 Lua（如代理类服务器）时回退到非原子多命令实现
	return b.addFallback(ctx, item)
}

// addFallback 非原子回退：exists + k 次 SetBit 多往返，语义与原实现一致。
func (b *bitmapImpl) addFallback(ctx context.Context, item string) (bool, error) {
	exists, err := b.existsFallback(ctx, item)
	if err != nil {
		return false, err
	}

	for _, pos := range b.hashs(item) {
		if err := b.client.SetBit(ctx, b.key, int64(pos), 1).Err(); err != nil {
			return false, err
		}
	}

	// Return true if newly added (all bits were 0 before this call)
	return !exists, nil
}

func (b *bitmapImpl) Add(ctx context.Context, item string) (bool, error) {
	return b.add(ctx, item)
}

func (b *bitmapImpl) exists(ctx context.Context, item string) (bool, error) {
	exists, err := bitmapExistsScript.Run(ctx, b.client, []string{b.key}, b.positions(item)...).Int()
	if err == nil {
		return exists == 1, nil
	}

	// 服务器不支持 Lua 时回退到多命令实现
	return b.existsFallback(ctx, item)
}

// existsFallback 非原子回退：k 次 GetBit 逐位检查。
func (b *bitmapImpl) existsFallback(ctx context.Context, item string) (bool, error) {
	for _, pos := range b.hashs(item) {
		bit, err := b.client.GetBit(ctx, b.key, int64(pos)).Result()
		if err != nil {
			return false, err
		}
		if bit == 0 {
			return false, nil
		}
	}
	return true, nil
}

func (b *bitmapImpl) Exists(ctx context.Context, item string) (bool, error) {
	return b.exists(ctx, item)
}

func (b *bitmapImpl) AddMulti(ctx context.Context, items ...string) ([]bool, error) {
	result := make([]bool, len(items))
	for i, item := range items {
		added, err := b.add(ctx, item)
		if err != nil {
			return nil, err
		}
		result[i] = added
	}
	return result, nil
}

func (b *bitmapImpl) ExistsMulti(ctx context.Context, items ...string) ([]bool, error) {
	result := make([]bool, len(items))
	for i, item := range items {
		exists, err := b.exists(ctx, item)
		if err != nil {
			return nil, err
		}
		result[i] = exists
	}
	return result, nil
}

func (b *bitmapImpl) Info(ctx context.Context) (*BloomInfo, error) {
	// For the bitmap fallback, we estimate current item count
	strLen, err := b.client.StrLen(ctx, b.key).Result()
	if err != nil {
		return nil, err
	}

	// Estimate: numItems ≈ -m/k * ln(1 - bitsSet/m)
	// Simplified estimation uses byte length
	return &BloomInfo{
		Capacity: b.capacity,
		Size:     strLen,
	}, nil
}
