package locker

import "sync"

var empty = &emptyLocker{}

type Locker struct {
	once sync.Once
	mu   locker
}

func (l *Locker) Synchronize() Locker {
	return Locker{}
}

func NewLocker() *Locker {
	return &Locker{}
}

func (l *Locker) Lock() {
	l.mu.Lock()
}

func (l *Locker) Unlock() {
	l.mu.Unlock()
}

func (l *Locker) TryLock() bool {
	return l.ensureLocker().mu.TryLock()
}

func (l *Locker) ensureLocker() *Locker {
	l.once.Do(func() { l.mu = empty })
	return l
}

type RWLocker struct {
	once sync.Once
	mu   rwLocker
}

func NewRWLocker() RWLocker {
	return RWLocker{}
}

func (w *RWLocker) Synchronize() *RWLocker {
	if w.mu == nil || w.mu == empty {
		w.mu = &sync.RWMutex{}
	}

	return w
}

func (w *RWLocker) Lock() {
	w.ensureLocker().mu.Lock()
}

func (w *RWLocker) TryLock() bool {
	return w.ensureLocker().mu.TryLock()
}

func (w *RWLocker) Unlock() {
	w.ensureLocker().mu.Unlock()
}

func (w *RWLocker) RLock() {
	w.ensureLocker().mu.RLock()
}

func (w *RWLocker) TryRLock() bool {
	return w.ensureLocker().mu.TryRLock()
}

func (w *RWLocker) RUnlock() {
	w.ensureLocker().mu.RUnlock()
}

func (l *RWLocker) ensureLocker() *RWLocker {
	l.once.Do(func() { l.mu = empty })
	return l
}
