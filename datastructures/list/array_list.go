package list

const minCapacity = 16

type ArrayList[T any] struct {
	buf    []T
	head   int
	tail   int
	minCap int
	list[T]
}

func NewArrayList[T any](elems ...T) *ArrayList[T] {
	minCap := minCapacity

	size := len(elems)
	for minCap < size {
		minCap <<= 1
	}

	var tail int = size
	var buf []T

	if len(elems) > 0 {
		buf = make([]T, minCap)
		copy(buf, elems)
	}

	l := &ArrayList[T]{
		list:   list[T]{size: size},
		buf:    buf,
		tail:   tail,
		minCap: minCap,
	}

	// for _, v := range elems {
	// 	l.PushBack(v)
	// }

	return l
}

func (l *ArrayList[T]) PushFront(v T) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.grow()

	l.head = l.prev(l.head)
	l.buf[l.head] = v
	l.size++
}

func (l *ArrayList[T]) PushBack(v T) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.grow()

	l.buf[l.tail] = v

	l.tail = l.next(l.tail)
	l.size++
}

func (l *ArrayList[T]) PopFront() T {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.size <= 0 {
		panic("list: PopFront() called on empty list")
	}
	ret := l.buf[l.head]
	var zero T
	l.buf[l.head] = zero

	l.head = l.next(l.head)
	l.size--

	l.shrink()
	return ret
}

func (l *ArrayList[T]) PopBack() T {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.tail = l.prev(l.tail)

	ret := l.buf[l.tail]
	var zero T
	l.buf[l.tail] = zero
	l.size--

	l.shrink()
	return ret
}

func (l *ArrayList[T]) RemoveAt(at int) T {
	if at < 0 || at >= l.Size() {
		panic(ErrorOutOffRange)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	rm := (l.head + at) & (len(l.buf) - 1)
	if at*2 < l.size {
		for i := 0; i < at; i++ {
			prev := l.prev(rm)
			l.buf[prev], l.buf[rm] = l.buf[rm], l.buf[prev]
			rm = prev
		}
		return l.PopFront()
	}
	swaps := l.size - at - 1
	for i := 0; i < swaps; i++ {
		next := l.next(rm)
		l.buf[rm], l.buf[next] = l.buf[next], l.buf[rm]
		rm = next
	}
	return l.PopBack()
}

func (l *ArrayList[T]) Front() T {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.buf[l.head]
}

func (l *ArrayList[T]) Back() T {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.buf[l.tail]
}

func (l *ArrayList[T]) ForEach(fn func(T)) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	n := l.head
	for i := 0; i < l.size; i++ {
		fn(l.buf[n])

		n = l.next(n)
	}
}

func (q *ArrayList[T]) prev(i int) int {
	return (i - 1) & (len(q.buf) - 1)
}

func (l *ArrayList[T]) next(i int) int {
	return (i + 1) & (len(l.buf) - 1)
}

func (l *ArrayList[T]) grow() {
	if l.size != len(l.buf) {
		return
	}
	if len(l.buf) == 0 {
		if l.minCap == 0 {
			l.minCap = minCapacity
		}
		l.buf = make([]T, l.minCap)
		return
	}

	l.resize()
}

func (l *ArrayList[T]) shrink() {
	if len(l.buf) > l.minCap && (l.size<<2) == len(l.buf) {
		l.resize()
	}
}

// resize resizes the list to fit exactly twice its current contents. This is
// used to grow the list when it is full, and also to shrink it when it is
// only a quarter full.
func (l *ArrayList[T]) resize() {
	newBuf := make([]T, l.size<<1)
	if l.tail > l.head {
		copy(newBuf, l.buf[l.head:l.tail])
	} else {
		n := copy(newBuf, l.buf[l.head:])
		copy(newBuf[n:], l.buf[:l.tail])
	}

	l.head = 0
	l.tail = l.size
	l.buf = newBuf
}
