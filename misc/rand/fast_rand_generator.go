package rand

import (
	_ "unsafe"
)

type fastRandGenerator struct {
}

func (*fastRandGenerator) Int63() int64 {
	uu := uint64(uint64(Fastrand())<<32) ^ uint64(Fastrand())
	return int64(uu << 1 >> 1)
}

func (r *fastRandGenerator) Int63n(n int64) int64 {
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

func (*fastRandGenerator) Int31() int32 {
	u := Fastrand()
	return int32(u << 1 >> 1)
}

func (r fastRandGenerator) Int31n(n int32) int32 {
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

func (*fastRandGenerator) Int() int {
	u := Fastrand()
	return int(u << 1 >> 1)
}

func (r *fastRandGenerator) Intn(n int) int {
	if n <= 0 {
		panic("invalid argument to Intn")
	}
	if n <= 1<<31-1 {
		return int(r.Int31n(int32(n)))
	}

	return int(r.Int63n(int64(n)))
}

//go:linkname Fastrand runtime.fastrand
func Fastrand() uint32
