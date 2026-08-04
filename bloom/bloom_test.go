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

func TestBloomExistMulti(t *testing.T) {
	bf := NewOptimal(100000, 0.0001)
	ctx := context.Background()
	
	// Add some elements
	testData := []string{"apple", "banana", "cherry", "date", "elderberry"}
	bf.AddMulti(ctx, testData...)
	
	// Test ExistMulti with mixed existing and non-existing elements
	// Use longer random strings to avoid false positives
	checkData := []string{"apple", "xyz123abc456def789", "banana", "qwerty987654321", "cherry"}
	results := bf.ExistMulti(ctx, checkData...)
	
	// Verify results
	expected := []bool{true, false, true, false, true}
	if len(results) != len(expected) {
		t.Fatalf("Expected %d results, got %d", len(expected), len(results))
	}
	
	for i, exp := range expected {
		if results[i] != exp {
			t.Errorf("Element %s: expected %v, got %v", checkData[i], exp, results[i])
		}
	}
	
	// Test with empty input
	emptyResults := bf.ExistMulti(ctx)
	if len(emptyResults) != 0 {
		t.Errorf("Expected empty results for empty input, got %d results", len(emptyResults))
	}
	
	// Test with all existing elements
	allExist := bf.ExistMulti(ctx, "apple", "banana", "cherry")
	for i, exists := range allExist {
		if !exists {
			t.Errorf("Expected element %d to exist", i)
		}
	}
	
	// Test with all non-existing elements
	noneExist := bf.ExistMulti(ctx, "fig", "grape", "kiwi")
	for i, exists := range noneExist {
		if exists {
			t.Errorf("Expected element %d to not exist", i)
		}
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
