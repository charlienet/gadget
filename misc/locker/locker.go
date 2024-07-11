package locker

type locker interface {
	Lock()
	TryLock() bool
	Unlock()
}

type rwLocker interface {
	locker
	RLock()
	RUnlock()
	TryRLock() bool
}
