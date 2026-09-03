package lock

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type fakeEntry struct {
	token string
	ttl   time.Duration
}

type fakeBackend struct {
	mu   sync.Mutex
	keys map[string]fakeEntry
	err  error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{keys: map[string]fakeEntry{}}
}

func (f *fakeBackend) TryAcquire(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.keys[key]; ok && e.ttl > 0 {
		return false, nil
	}
	f.keys[key] = fakeEntry{token: token, ttl: ttl}
	return true, nil
}

func (f *fakeBackend) Release(ctx context.Context, key, token string) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.keys[key]; ok && e.token == token {
		delete(f.keys, key)
	}
	return nil
}

func (f *fakeBackend) Renew(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.keys[key]; ok && e.token == token {
		f.keys[key] = fakeEntry{token: token, ttl: ttl}
		return true, nil
	}
	return false, nil
}

// noRenewBackend 是无续期能力后端：嵌入 Backend 屏蔽 Renewer 断言
type noRenewBackend struct {
	Backend
}

func newFakeUnavailable() *fakeBackend {
	f := newFakeBackend()
	f.err = fmt.Errorf("%w: %v", ErrBackendUnavailable, fmt.Errorf("simulated outage"))
	return f
}
