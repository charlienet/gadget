package rand_test

import (
	"encoding/hex"
	"testing"

	"github.com/charlienet/gadget/misc/rand"
)

func TestRandString(t *testing.T) {
	for i := 0; i < 100; i++ {
		t.Log(rand.Int[int64]())
		t.Log(rand.IntRange(15, 100))

		b, _ := rand.RandBytes(24)
		t.Log(hex.EncodeToString(b))

		rand.FastGenerator.Int()

	}
}

func TestNormalGenerator(t *testing.T) {
	rand.NormalGenerator.Int31()
}

func BenchmarkGenerate(b *testing.B) {
	b.Run("fast", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			rand.FastGenerator.Int31()
		}
	})

	b.Run("normal", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			rand.NormalGenerator.Int31()
		}
	})

	b.Run("secure", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			rand.SecureGenerator.Int31()
		}
	})
}
