package lock

import (
	"context"
	"errors"
	"testing"
	"time"
)

// --- 互斥测试 ---

func TestTryLock_Mutex(t *testing.T) {
	b := newFakeBackend()
	l1 := New("test-key", WithBackend(b))
	l2 := New("test-key", WithBackend(b))

	ok1, err := l1.TryLock(context.Background())
	if err != nil || !ok1 {
		t.Fatalf("l1.TryLock: ok=%v, err=%v", ok1, err)
	}

	ok2, err := l2.TryLock(context.Background())
	if err != nil {
		t.Fatalf("l2.TryLock unexpected error: %v", err)
	}
	if ok2 {
		t.Fatal("l2.TryLock should return false (lock held by l1)")
	}
}

func TestTryLock_AfterUnlock(t *testing.T) {
	b := newFakeBackend()
	l1 := New("test-key", WithBackend(b))
	l2 := New("test-key", WithBackend(b))

	ok1, err := l1.TryLock(context.Background())
	if err != nil || !ok1 {
		t.Fatalf("l1.TryLock: ok=%v, err=%v", ok1, err)
	}

	if err := l1.Unlock(context.Background()); err != nil {
		t.Fatalf("l1.Unlock: %v", err)
	}

	ok2, err := l2.TryLock(context.Background())
	if err != nil || !ok2 {
		t.Fatalf("l2.TryLock after unlock: ok=%v, err=%v", ok2, err)
	}
}

// --- 防误删测试 ---

func TestUnlock_NonOwner(t *testing.T) {
	b := newFakeBackend()
	owner := New("test-key", WithBackend(b))
	other := New("test-key", WithBackend(b))

	ok, err := owner.TryLock(context.Background())
	if err != nil || !ok {
		t.Fatalf("owner.TryLock: ok=%v, err=%v", ok, err)
	}

	// 非持有者尝试释放 — 后端静默返回 nil
	if err := other.Unlock(context.Background()); err != nil {
		t.Fatalf("non-owner Unlock should be nil, got: %v", err)
	}

	// 锁仍然存在
	ok2, err := owner.TryLock(context.Background())
	if err != nil {
		t.Fatalf("owner re-try after non-owner unlock: err=%v", err)
	}
	if ok2 {
		t.Fatal("lock should still be held, re-try should fail")
	}
}

func TestUnlock_OwnerReacquire(t *testing.T) {
	b := newFakeBackend()
	l := New("test-key", WithBackend(b))

	ok, err := l.TryLock(context.Background())
	if err != nil || !ok {
		t.Fatalf("TryLock: ok=%v, err=%v", ok, err)
	}

	if err := l.Unlock(context.Background()); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	// 持有者释放后可再获取
	ok2, err := l.TryLock(context.Background())
	if err != nil || !ok2 {
		t.Fatalf("re-acquire after unlock: ok=%v, err=%v", ok2, err)
	}
}

// --- 阻塞 Lock 测试 ---

func TestLock_BlockedThenReleased(t *testing.T) {
	b := newFakeBackend()
	hold := New("test-key", WithBackend(b))

	ok, err := hold.TryLock(context.Background())
	if err != nil || !ok {
		t.Fatalf("hold.TryLock: ok=%v, err=%v", ok, err)
	}

	var gotErr error
	done := make(chan struct{})

	go func() {
		defer close(done)
		waiter := New("test-key", WithBackend(b), WithRetryInterval(10*time.Millisecond))
		gotErr = waiter.Lock(context.Background())
	}()

	// 等待 waiter 进入阻塞状态
	time.Sleep(30 * time.Millisecond)

	// 释放锁
	if err := hold.Unlock(context.Background()); err != nil {
		t.Fatalf("hold.Unlock: %v", err)
	}

	// 等待 waiter 完成
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return after lock release")
	}

	if gotErr != nil {
		t.Fatalf("waiter.Lock should return nil, got: %v", gotErr)
	}
}

func TestLock_ContextCancelled(t *testing.T) {
	b := newFakeBackend()
	hold := New("test-key", WithBackend(b))

	ok, err := hold.TryLock(context.Background())
	if err != nil || !ok {
		t.Fatalf("hold.TryLock: ok=%v, err=%v", ok, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		waiter := New("test-key", WithBackend(b), WithRetryInterval(10*time.Millisecond))
		done <- waiter.Lock(ctx)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case gotErr := <-done:
		if !errors.Is(gotErr, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", gotErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return after context cancel")
	}
}

// --- 超时测试 ---

func TestLock_Timeout(t *testing.T) {
	b := newFakeBackend()
	hold := New("test-key", WithBackend(b))

	ok, err := hold.TryLock(context.Background())
	if err != nil || !ok {
		t.Fatalf("hold.TryLock: ok=%v, err=%v", ok, err)
	}

	waiter := New("test-key", WithBackend(b),
		WithRetryInterval(10*time.Millisecond),
		WithTimeout(50*time.Millisecond),
	)

	gotErr := waiter.Lock(context.Background())
	if !errors.Is(gotErr, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", gotErr)
	}
}

// --- Renew 测试 ---

func TestRenew_Owner(t *testing.T) {
	b := newFakeBackend()
	l := New("test-key", WithBackend(b))

	ok, err := l.TryLock(context.Background())
	if err != nil || !ok {
		t.Fatalf("TryLock: ok=%v, err=%v", ok, err)
	}

	renewed, err := l.Renew(context.Background(), 30*time.Second)
	if err != nil {
		t.Fatalf("Renew error: %v", err)
	}
	if !renewed {
		t.Fatal("expected Renew to succeed for owner")
	}
}

func TestRenew_NonOwner(t *testing.T) {
	b := newFakeBackend()
	owner := New("test-key", WithBackend(b))
	other := New("test-key", WithBackend(b))

	ok, err := owner.TryLock(context.Background())
	if err != nil || !ok {
		t.Fatalf("owner.TryLock: ok=%v, err=%v", ok, err)
	}

	renewed, err := other.Renew(context.Background(), 30*time.Second)
	if err != nil {
		t.Fatalf("non-owner Renew error: %v", err)
	}
	if renewed {
		t.Fatal("expected non-owner Renew to return false")
	}
}

func TestRenew_TTLNegative(t *testing.T) {
	b := newFakeBackend()
	l := New("test-key", WithBackend(b))

	renewed, err := l.Renew(context.Background(), 0)
	if err == nil {
		t.Fatal("expected error for ttl <= 0")
	}
	if renewed {
		t.Fatal("expected false for invalid ttl")
	}

	renewed, err = l.Renew(context.Background(), -1*time.Second)
	if err == nil {
		t.Fatal("expected error for negative ttl")
	}
}

func TestRenew_NoRenewer(t *testing.T) {
	inner := newFakeBackend()
	b := &noRenewBackend{inner}
	l := New("test-key", WithBackend(b))

	ok, err := l.TryLock(context.Background())
	if err != nil || !ok {
		t.Fatalf("TryLock: ok=%v, err=%v", ok, err)
	}

	renewed, err := l.Renew(context.Background(), 30*time.Second)
	if !errors.Is(err, ErrRenewUnsupported) {
		t.Fatalf("expected ErrRenewUnsupported, got: %v", err)
	}
	if renewed {
		t.Fatal("expected false when renew unsupported")
	}
}

// --- Token 测试 ---

func TestToken_Specified(t *testing.T) {
	b := newFakeBackend()
	l := New("test-key", WithBackend(b), WithToken("my-token"))
	if l.Token() != "my-token" {
		t.Fatalf("expected token 'my-token', got '%s'", l.Token())
	}
}

func TestToken_Random(t *testing.T) {
	b := newFakeBackend()
	l1 := New("test-key", WithBackend(b))
	l2 := New("test-key", WithBackend(b))

	if l1.Token() == "" {
		t.Fatal("token should not be empty")
	}
	if l1.Token() == l2.Token() {
		t.Fatal("two random tokens should differ")
	}
}
