package rand

import (
	"crypto/rand"
	"math/big"

	"github.com/charlienet/gadget/misc/bytesconv"
)

type secureRandGenerator struct{}

func (secureRandGenerator) Int() int {
	i, _ := bytesconv.LittleEndian.BytesToUInt64(read(4))
	return int(i << 1 >> 1)
}

func (s secureRandGenerator) Intn(max int) int {
	return int(s.Int63n(int64(max)))
}

func (secureRandGenerator) Int31() int32 {
	i, _ := bytesconv.LittleEndian.BytesToUInt64(read(4))
	return int32(uint32(i) << 1 >> 1)
}

func (s secureRandGenerator) Int31n(max int32) int32 {
	return int32(s.Int63n(int64(max)))
}

func (secureRandGenerator) Int63() int64 {
	i, _ := bytesconv.LittleEndian.BytesToUInt64(read(8))
	return int64(i << 1 >> 1)
}

func (secureRandGenerator) Int63n(max int64) int64 {
	n, _ := rand.Int(rand.Reader, big.NewInt(max))
	return n.Int64()
}

func read(n int) []byte {
	buf := make([]byte, n)
	rand.Read(buf)

	return buf
}
