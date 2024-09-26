package bloom

import (
	"context"
	"testing"

	"github.com/charlienet/go-misc/random"
)

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
			bf.Exist(ctx, random.Hex.Generate(2))
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
