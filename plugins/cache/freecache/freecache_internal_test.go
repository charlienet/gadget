package freecache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreMeta(t *testing.T) {
	s := new(1000)

	assert.Equal(t, "freecache", s.Name())
	assert.False(t, s.IsRemote())
}

func TestStoreTTLZeroNeverExpires(t *testing.T) {
	s := new(1000)
	ctx := context.Background()

	require.NoError(t, s.Put(ctx, "key", []byte("value"), 0))

	<-time.After(1500 * time.Millisecond)

	v, ok, err := s.Get(ctx, "key")
	require.NoError(t, err)
	assert.True(t, ok, "entry with expireSeconds=0 must not expire")
	assert.Equal(t, []byte("value"), v)
}

func TestStoreDeleteIdempotent(t *testing.T) {
	s := new(1000)
	ctx := context.Background()

	require.NoError(t, s.Put(ctx, "key", []byte("value"), 0))
	require.NoError(t, s.Delete(ctx, "key"))
	// Deleting a missing key must be a no-op, not an error.
	require.NoError(t, s.Delete(ctx, "key"))
	require.NoError(t, s.Delete(ctx, "missing", "another"))

	_, ok, err := s.Get(ctx, "key")
	require.NoError(t, err)
	assert.False(t, ok, "key must be deleted")
}

// TestNewRejectsNonPositiveSize verifies the size <= 0 validation introduced
// with the (cache.Option, error) constructor signature.
func TestNewRejectsNonPositiveSize(t *testing.T) {
	for _, size := range []int{0, -1, -100} {
		opt, err := New(size)
		require.Error(t, err, "New(%d) should return an error", size)
		assert.Nil(t, opt, "New(%d) should return a nil option on error", size)
	}
}

// TestConcurrentPutGetDelete is a smoke test for concurrent Put/Get/Delete on
// the same key: it must not panic, hits must carry non-empty values (the last
// writer wins, no exact value assertion), and a final Delete must yield a
// stable miss. Must pass with -race.
func TestConcurrentPutGetDelete(t *testing.T) {
	s := new(1024 * 1024) // 1MB, comfortably above the 512KB promotion floor
	ctx := context.Background()

	const (
		goroutines = 20
		rounds     = 50
	)
	key := "contended-key"

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				switch r % 3 {
				case 0, 1:
					val := []byte(fmt.Sprintf("v-%d-%d", id, r))
					if err := s.Put(ctx, key, val, 0); err != nil {
						t.Errorf("goroutine %d Put: %v", id, err)
						return
					}
				case 2:
					if err := s.Delete(ctx, key); err != nil {
						t.Errorf("goroutine %d Delete: %v", id, err)
						return
					}
				}

				v, ok, err := s.Get(ctx, key)
				if err != nil {
					t.Errorf("goroutine %d Get: %v", id, err)
					return
				}
				if ok && len(v) == 0 {
					t.Errorf("goroutine %d: hit with empty value", id)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	// Concurrent phase over: Delete must yield a stable miss.
	require.NoError(t, s.Delete(ctx, key))
	_, ok, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.False(t, ok, "expected miss after final Delete")
}
