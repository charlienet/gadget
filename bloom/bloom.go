package bloom

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"math"

	"github.com/charlienet/go-crypto/hash"
	"github.com/charlienet/go-misc/pool"
)

type BloomFilter interface {
}

type bloom_filter struct {
	mem      *mem_store
	store    Store
	funs     []uint64
	size     uint64
	hashSize int
}

func New(capacity uint, fpp float64, opts ...option) *bloom_filter {
	m, k := optimalK(capacity, fpp)
	keys := make([]uint64, k)
	if err := binary.Read(rand.Reader, binary.LittleEndian, keys); err != nil {
		panic(err)
	}

	opt := Optons{}
	for _, o := range opts {
		o(&opt)
	}

	ctx := context.Background()

	// Initialize distributed storage
	if opt.store != nil {
		if s, ok := opt.store.(interface {
			Initialize(context.Context, []uint64, uint, float64) []uint64
		}); ok {
			keys = s.Initialize(ctx, keys, capacity, fpp)
		}
	}

	return &bloom_filter{funs: keys, mem: newMemStore(m), store: opt.store, size: m, hashSize: int(k)}
}

func (bf *bloom_filter) Add(ctx context.Context, data string) {
	offsets := bf.getOffsets(data)
	bf.mem.Set(ctx, offsets)

	if bf.store != nil {
		bf.store.Add(ctx, data, offsets)
	}
}

func (bf *bloom_filter) Exist(ctx context.Context, data string) bool {
	if len(data) == 0 {
		return false
	}

	offsets := bf.getOffsets(data)
	exist := bf.mem.Test(ctx, offsets)

	// Check for the presence of remote storage when local does not exist
	if !exist && bf.store != nil {
		exist = bf.store.Test(ctx, data, offsets)
		if exist {
			bf.mem.Set(ctx, offsets)
		}
	}

	return exist
}

func (bf *bloom_filter) Clear(ctx context.Context) {
	bf.mem.Clear(ctx)
	if bf.store != nil {
		bf.store.Clear(ctx)
	}
}

var p = pool.New(func() []uint64 { return make([]uint64, 20) })

func (bf *bloom_filter) getOffsets(data string) []uint64 {
	// offsets := make([]uint64, bf.hashSize)

	offsets := p.Get()
	defer p.Put(offsets)

	sum := hash.Murmur3([]byte(data))
	for i := 0; i < bf.hashSize; i++ {
		offsets[i] = (sum ^ bf.funs[i]) % bf.size
	}

	return offsets[:bf.hashSize]
}

// 计算优化的位图长度
// n 期望放置元素数量
// p 预期的误判概率
// https://hur.st/bloomfilter/?n=10M&p=0.000001&m=&k=8
// m = ceil((n * log(p)) / log(1 / pow(2, log(2))));
func optimalK(n uint, p float64) (m, k uint64) {
	m = uint64(math.Ceil(-float64(n) * math.Log(p) / math.Pow(math.Ln2, 2)))
	k = uint64(math.Ceil(float64(m) * math.Ln2 / float64(n)))

	return m, k
}
