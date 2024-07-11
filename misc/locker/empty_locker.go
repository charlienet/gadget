package locker

type emptyLocker struct {
}

func (l *emptyLocker) RLock() {}

func (l *emptyLocker) RUnlock() {}

func (l *emptyLocker) Lock() {}

func (l *emptyLocker) Unlock() {}

func (l *emptyLocker) TryLock() bool { return true }

func (l *emptyLocker) TryRLock() bool { return true }
