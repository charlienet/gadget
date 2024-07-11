package rand

import (
	"sync"
	"time"

	mrnd "math/rand"
)

// 随机数生成器接口
type RandGenerator interface {
	Int() int
	Intn(int) int
	Int31() int32
	Int31n(int32) int32
	Int63() int64
	Int63n(int64) int64
}

type rangeGenerator struct {
	g RandGenerator
}

func NewRangeGenerator(g RandGenerator) *rangeGenerator {
	return &rangeGenerator{g: g}
}

func (g rangeGenerator) Intr(min, max int) int {
	n := max - min
	return min + g.g.Intn(n)
}

func (g rangeGenerator) Int31r(min, max int32) int32 {
	n := max - min
	return min + g.g.Int31n(n)
}

func (g rangeGenerator) Int63r(min, max int64) int64 {
	n := max - min
	return min + g.g.Int63n(n)
}

var (
	seed  = time.Now().UnixNano()                 // 随机数种子
	souce = mrnd.NewSource(time.Now().UnixNano()) // 用于初始化的随机数生成器
)

type mathRandGenerator struct {
	source mrnd.Source
	r      *sync.Mutex
}

func NewRandGenerator() *mathRandGenerator {
	return &mathRandGenerator{
		source: mrnd.NewSource(getSeed()),
		r:      &sync.Mutex{},
	}
}

func getSeed() int64 {
	seed ^= souce.Int63()
	return seed
}

func (r *mathRandGenerator) Int31() int32 {
	return int32(r.Int63() >> 32)
}

func (r *mathRandGenerator) Int31n(n int32) int32 {
	if n <= 0 {
		panic("invalid argument to Int31n")
	}

	if n&(n-1) == 0 { // n is power of two, can mask
		return r.Int31() & (n - 1)
	}
	max := int32((1 << 31) - 1 - (1<<31)%uint32(n))
	v := r.Int31()
	for v > max {
		v = r.Int31()
	}
	return v % n
}

func (r *mathRandGenerator) Int() int {
	u := uint(r.Int63())
	return int(u << 1 >> 1) // clear sign bit if int == int32
}

// Intn returns, as an int, a non-negative pseudo-random number in the half-open interval [0,n).
// It panics if n <= 0.
func (r *mathRandGenerator) Intn(n int) int {
	if n <= 0 {
		panic("invalid argument to Intn")
	}
	if n <= 1<<31-1 {
		return int(r.Int31n(int32(n)))
	}

	return int(r.Int63n(int64(n)))
}

// Int63n returns, as an int64, a non-negative pseudo-random number in the half-open interval [0,n).
// It panics if n <= 0.
func (r *mathRandGenerator) Int63n(n int64) int64 {
	if n <= 0 {
		panic("invalid argument to Int63n")
	}
	if n&(n-1) == 0 { // n is power of two, can mask
		return r.Int63() & (n - 1)
	}
	max := int64((1 << 63) - 1 - (1<<63)%uint64(n))
	v := r.Int63()
	for v > max {
		v = r.Int63()
	}

	return v % n
}

func (r *mathRandGenerator) Int63() int64 {
	r.r.Lock()
	i := r.source.Int63()
	r.r.Unlock()

	return i
}
