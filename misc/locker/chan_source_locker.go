package locker

import "sync"

type chanSourceLocker struct {
	m       sync.RWMutex
	context map[string]chan int
}

func NewChanSourceLocker() *chanSourceLocker {
	return &chanSourceLocker{
		context: make(map[string]chan int),
	}
}

func (s *chanSourceLocker) Lock(key string) (ok bool, ch <-chan int) {
	s.m.RLock()
	ch, ok = s.context[key]
	s.m.RUnlock()
	if ok {
		return
	}

	s.m.Lock()
	if ch, ok = s.context[key]; ok {
		s.m.Unlock()
		return
	}

	s.context[key] = make(chan int)
	ch = s.context[key]
	s.m.Unlock()

	return

}

func (s *chanSourceLocker) Unlock(key string) {
	s.m.Lock()
	defer s.m.Unlock()

	if ch, ok := s.context[key]; ok {
		close(ch)
		delete(s.context, key)
	}
}
