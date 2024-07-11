package locker

import "sync"

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

func NewLocker() Locker {
	return Locker{
		mu: &sync.RWMutex{},
	}
}

func NewRWLocker() RWLocker {
	return RWLocker{
		mu: &sync.RWMutex{},
	}
}
