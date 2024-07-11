package locker

import "sync"

type ChanSourceLocker struct {
	m       sync.RWMutex
	content map[string]chan int
}

func NewChanSourceLocker() *ChanSourceLocker {
	return &ChanSourceLocker{
		content: make(map[string]chan int),
	}
}

func (s *ChanSourceLocker) Lock(key string) (ok bool, ch <-chan int) {
	s.m.RLock()
	ch, exist := s.content[key]
	s.m.RUnlock()
	if exist {
		return
	}
	s.m.Lock()
	ch, exist = s.content[key]
	if exist {
		s.m.Unlock()
		return
	}
	s.content[key] = make(chan int)
	ch = s.content[key]
	ok = true
	s.m.Unlock()
	return
}

func (s *ChanSourceLocker) Unlock(key string) {
	s.m.Lock()
	defer s.m.Unlock()

	if ch, ok := s.content[key]; ok {
		close(ch)
		delete(s.content, key)
	}
}
