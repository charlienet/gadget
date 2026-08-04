package bloom

import (
	"crypto/rand"
	"encoding/hex"
	"context"
	"testing"
)

func randomHex(n int) string {
	bytes := make([]byte, n/2+1) // Generate enough bytes
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	hexStr := hex.EncodeToString(bytes)
	return hexStr[:n] // Return only n characters
}

func TestBloom(t *testing.T) {
	bf := NewOptimal(1000, 0.0001)

	ctx := context.Background()
	bf.Add(ctx, "abc")
	t.Log(bf.Exist(ctx, "abc"))
	t.Log(bf.Exist(ctx, "bbb"))

	bf.Clear(ctx)
	t.Log(bf.Exist(ctx, "abc"))

	t.Run("offset", func(t *testing.T) {
		t.Logf("offset:%v", bf.getOffsets("abc"))
		t.Logf("offset:%v", bf.getOffsets("abc"))
	})
}

func BenchmarkBloom(b *testing.B) {
	bf := NewOptimal(10000, 0.0001)
	ctx := context.Background()
	b.Run("r", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			bf.Exist(ctx, randomHex(2))
		}
	})
}

func BenchmarkHash(b *testing.B) {
	bf := NewOptimal(1000, 0.0001)
	b.Run("r", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			bf.getOffsets("abc")
		}
	})
}
