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

func TestBloomMulti(t *testing.T) {
	bf := NewOptimal(1000, 0.0001)

	ctx := context.Background()
	
	// Test AddMulti functionality
	testData := []string{"element1", "element2", "element3", "element4", "element5"}
	bf.AddMulti(ctx, testData...)
	
	// Verify all elements exist
	for _, element := range testData {
		if !bf.Exist(ctx, element) {
			t.Errorf("Expected element %s to exist in bloom filter", element)
		}
	}
	
	// Verify non-existent element doesn't exist
	if bf.Exist(ctx, "nonexistent") {
		t.Error("Expected 'nonexistent' element to not exist in bloom filter")
	}
	
	// Test with empty input
	bf.AddMulti(ctx)
	
	// Test with single element
	bf.AddMulti(ctx, "single_element")
	if !bf.Exist(ctx, "single_element") {
		t.Error("Expected 'single_element' to exist in bloom filter")
	}
}

func BenchmarkAddMulti(b *testing.B) {
	bf := NewOptimal(10000, 0.0001)
	ctx := context.Background()
	elements := make([]string, 100)
	for i := range elements {
		elements[i] = randomHex(10)
	}
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.AddMulti(ctx, elements...)
	}
}

func BenchmarkAddSingle(b *testing.B) {
	bf := NewOptimal(10000, 0.0001)
	ctx := context.Background()
	elements := make([]string, 100)
	for i := range elements {
		elements[i] = randomHex(10)
	}
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, element := range elements {
			bf.Add(ctx, element)
		}
	}
}

func BenchmarkHash(b *testing.B) {
	bf := NewOptimal(1000, 0.0001)
	b.Run("r", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			bf.getOffsets("abc")
		}
	})
}
