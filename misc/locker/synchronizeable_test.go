package locker_test

import (
	"sync"
	"testing"

	"github.com/charlienet/gadget/misc/locker"
	"github.com/stretchr/testify/assert"
)

func TestLocker(t *testing.T) {
	l := locker.NewRWLocker()

	l.Lock()
	l.Lock()

	println("test")

	l.Unlock()
}

func TestRWLocker(t *testing.T) {
	l := locker.NewRWLocker()
	t.Log(l.TryLock())
	t.Log(l.TryLock())

	l.Synchronize()
	t.Log(l.TryLock())
	t.Log(l.TryLock())
}

func TestNoLocker(t *testing.T) {
	var l locker.RWLocker
	a := assert.New(t)

	a.True(l.TryLock())
	a.True(l.TryLock())

	l.Synchronize()

	a.True(l.TryLock())
	a.False(l.TryLock())
}

func TestSysRWLocker(t *testing.T) {
	var rw sync.RWMutex
	var rw2 sync.RWMutex

	t.Logf("%p", &rw)
	t.Logf("%p", &rw2)

	t.Log(rw.TryLock())
	t.Log(rw2.TryLock())
	t.Log(rw.TryLock())
	t.Log(rw2.TryLock())
}
