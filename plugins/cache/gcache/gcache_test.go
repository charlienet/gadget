package gcache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/charlienet/gadget/cache"
)

func TestStoreGetHitAndMiss(t *testing.T) {
	s, err := newGcache(10)
	if err != nil {
		t.Fatalf("newGcache: %v", err)
	}
	ctx := context.Background()

	// Miss: a missing key returns (nil, false, nil) without error.
	v, ok, err := s.Get(ctx, "missing")
	if err != nil {
		t.Fatalf("Get(missing): %v", err)
	}
	if ok {
		t.Error("expected miss for missing key")
	}
	if v != nil {
		t.Errorf("expected nil value for missing key, got %q", v)
	}

	// Hit.
	if err := s.Put(ctx, "key", []byte("value"), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	v, ok, err = s.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Error("expected hit for existing key")
	}
	if string(v) != "value" {
		t.Errorf("expected %q, got %q", "value", v)
	}
}

func TestStoreTTLZeroNeverExpires(t *testing.T) {
	s, err := newGcache(10)
	if err != nil {
		t.Fatalf("newGcache: %v", err)
	}
	ctx := context.Background()

	if err := s.Put(ctx, "key", []byte("value"), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}

	time.Sleep(1500 * time.Millisecond)

	v, ok, err := s.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Error("expected key with expireSeconds=0 to still exist")
	}
	if string(v) != "value" {
		t.Errorf("expected %q, got %q", "value", v)
	}
}

func TestStoreTTLPositiveExpires(t *testing.T) {
	s, err := newGcache(10)
	if err != nil {
		t.Fatalf("newGcache: %v", err)
	}
	ctx := context.Background()

	if err := s.Put(ctx, "key", []byte("value"), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}

	time.Sleep(1500 * time.Millisecond)

	if _, ok, err := s.Get(ctx, "key"); err != nil {
		t.Fatalf("Get: %v", err)
	} else if ok {
		t.Error("expected key with positive TTL to be expired")
	}
}

func TestStoreOverwriteWithTTLZeroClearsStaleExpiration(t *testing.T) {
	s, err := newGcache(10)
	if err != nil {
		t.Fatalf("newGcache: %v", err)
	}
	ctx := context.Background()

	// First write with a short TTL...
	if err := s.Put(ctx, "key", []byte("ttl"), 1); err != nil {
		t.Fatalf("Put with TTL: %v", err)
	}
	// ...then overwrite with expireSeconds=0 ("never expire"). The stale
	// expiration from the first Put must not survive the overwrite.
	if err := s.Put(ctx, "key", []byte("forever"), 0); err != nil {
		t.Fatalf("Put without TTL: %v", err)
	}

	time.Sleep(1500 * time.Millisecond)

	v, ok, err := s.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Error("expected overwritten key to persist beyond the original TTL")
	}
	if string(v) != "forever" {
		t.Errorf("expected %q, got %q", "forever", v)
	}
}

func TestStoreDeleteIdempotent(t *testing.T) {
	s, err := newGcache(10)
	if err != nil {
		t.Fatalf("newGcache: %v", err)
	}
	ctx := context.Background()

	if err := s.Put(ctx, "key", []byte("value"), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := s.Delete(ctx, "key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Deleting a missing key must be a no-op, not an error.
	if err := s.Delete(ctx, "key"); err != nil {
		t.Fatalf("second Delete should be idempotent: %v", err)
	}

	if _, ok, err := s.Get(ctx, "key"); err != nil {
		t.Fatalf("Get: %v", err)
	} else if ok {
		t.Error("expected key to be deleted")
	}
}

func TestNewRejectsNonPositiveSize(t *testing.T) {
	for _, size := range []int{0, -1, -100} {
		opt, err := New(size)
		if err == nil {
			t.Errorf("New(%d) should return an error", size)
		}
		if opt != nil {
			t.Errorf("New(%d) should return a nil option on error", size)
		}
	}
}

func TestStoreMeta(t *testing.T) {
	s, err := newGcache(10)
	if err != nil {
		t.Fatalf("newGcache: %v", err)
	}

	if got := s.Name(); got != "gcache" {
		t.Errorf("Name() = %q, want %q", got, "gcache")
	}
	if s.IsRemote() {
		t.Error("IsRemote() should be false for a local cache")
	}
}

// TestCacheOptionIntegration exercises the store end-to-end through the cache
// package (serialization + version wrapping).
func TestCacheOptionIntegration(t *testing.T) {
	opt, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c := cache.New(opt)
	ctx := context.TODO()

	if err := c.Put(ctx, "key", "value", 0); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var s string
	if err := c.Get(ctx, "key", &s); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s != "value" {
		t.Errorf("expected %q, got %q", "value", s)
	}

	// Miss must surface cache.ErrEntityNotExist.
	if err := c.Get(ctx, "missing", &s); !errors.Is(err, cache.ErrEntityNotExist) {
		t.Errorf("expected cache.ErrEntityNotExist, got %v", err)
	}
}

// TestConcurrentPutGetDelete is a smoke test for concurrent Put/Get/Delete on
// the same key: it must not panic, hits must carry non-empty values (the last
// writer wins, no exact value assertion), and a final Delete must yield a
// stable miss. Must pass with -race.
func TestConcurrentPutGetDelete(t *testing.T) {
	s, err := newGcache(4096)
	if err != nil {
		t.Fatalf("newGcache: %v", err)
	}
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
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("final Delete: %v", err)
	}
	if _, ok, err := s.Get(ctx, key); err != nil {
		t.Fatalf("Get after final Delete: %v", err)
	} else if ok {
		t.Error("expected miss after final Delete")
	}
}
