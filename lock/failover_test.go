package lock

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// --- FailClosed 兜底矩阵 ---

func TestFailover_FailClosed_TryLock(t *testing.T) {
	b := newFakeUnavailable()
	l := New("test-key", WithBackend(b), WithFailPolicy(FailClosed))

	ok, err := l.TryLock(context.Background())
	if ok {
		t.Fatal("expected false")
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("expected ErrBackendUnavailable, got: %v", err)
	}
}

func TestFailover_FailClosed_Lock(t *testing.T) {
	b := newFakeUnavailable()
	l := New("test-key", WithBackend(b), WithFailPolicy(FailClosed))

	err := l.Lock(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("expected ErrBackendUnavailable, got: %v", err)
	}
}

func TestFailover_FailClosed_Unlock(t *testing.T) {
	b := newFakeUnavailable()
	l := New("test-key", WithBackend(b), WithFailPolicy(FailClosed))

	err := l.Unlock(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("expected ErrBackendUnavailable, got: %v", err)
	}
}

func TestFailover_FailClosed_Renew(t *testing.T) {
	b := newFakeUnavailable()
	l := New("test-key", WithBackend(b), WithFailPolicy(FailClosed))

	renewed, err := l.Renew(context.Background(), 30*time.Second)
	if renewed {
		t.Fatal("expected false")
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("expected ErrBackendUnavailable, got: %v", err)
	}
}

// --- FailOpen 兜底矩阵 ---

func TestFailover_FailOpen_TryLock(t *testing.T) {
	b := newFakeUnavailable()
	l := New("test-key", WithBackend(b), WithFailPolicy(FailOpen))

	ok, err := l.TryLock(context.Background())
	if !ok {
		t.Fatal("expected true")
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("expected ErrBackendUnavailable, got: %v", err)
	}
}

func TestFailover_FailOpen_Lock(t *testing.T) {
	b := newFakeUnavailable()
	l := New("test-key", WithBackend(b), WithFailPolicy(FailOpen))

	err := l.Lock(context.Background())
	if err != nil {
		t.Fatalf("expected nil for FailOpen, got: %v", err)
	}
}

func TestFailover_FailOpen_Unlock(t *testing.T) {
	b := newFakeUnavailable()
	l := New("test-key", WithBackend(b), WithFailPolicy(FailOpen))

	err := l.Unlock(context.Background())
	if err != nil {
		t.Fatalf("expected nil for FailOpen, got: %v", err)
	}
}

func TestFailover_FailOpen_Renew(t *testing.T) {
	b := newFakeUnavailable()
	l := New("test-key", WithBackend(b), WithFailPolicy(FailOpen))

	renewed, err := l.Renew(context.Background(), 30*time.Second)
	if !renewed {
		t.Fatal("expected true for FailOpen")
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("expected ErrBackendUnavailable, got: %v", err)
	}
}

// --- 非可用错误：原样透传 ---

type plainErrorBackend struct {
	Backend
}

func (p *plainErrorBackend) TryAcquire(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	return false, fmt.Errorf("some command-level error")
}

func (p *plainErrorBackend) Release(ctx context.Context, key, token string) error {
	return fmt.Errorf("some command-level error")
}

func (p *plainErrorBackend) Renew(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	return false, fmt.Errorf("some command-level error")
}

func TestNonAvailableError_PlainError(t *testing.T) {
	inner := newFakeBackend()
	b := &plainErrorBackend{inner}
	l := New("test-key", WithBackend(b), WithFailPolicy(FailClosed))

	// TryLock: 原样透传，不兜底
	ok, err := l.TryLock(context.Background())
	if ok {
		t.Fatal("expected false")
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrBackendUnavailable) {
		t.Fatal("plain error should NOT match ErrBackendUnavailable")
	}

	// Unlock: 原样透传
	if err := l.Unlock(context.Background()); err == nil {
		t.Fatal("expected error")
	} else if errors.Is(err, ErrBackendUnavailable) {
		t.Fatal("plain error should NOT match ErrBackendUnavailable")
	}

	// Renew: 原样透传
	renewed, err := l.Renew(context.Background(), 30*time.Second)
	if renewed {
		t.Fatal("expected false")
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrBackendUnavailable) {
		t.Fatal("plain error should NOT match ErrBackendUnavailable")
	}
}

// --- New panic when backend missing ---

func TestNew_PanicWithoutBackend(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when no backend configured")
		}
	}()
	_ = New("test-key")
}
