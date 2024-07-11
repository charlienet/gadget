package queue

import "sync"

type Queue[T any] interface {
	Put(T)
	Poll() T
	Peek() T
	Size() int
	IsEmpty() bool
}

var _ Queue[string] = &ArrayQueue[string]{}

// 数组队列，先进先出
type ArrayQueue[T any] struct {
	array []T        // 底层切片
	size  int        // 队列的元素数量
	lock  sync.Mutex // 为了并发安全使用的锁
}

func NewArrayQueue[T any]() *ArrayQueue[T] {
	return &ArrayQueue[T]{}
}

// 入队
func (q *ArrayQueue[T]) Put(v T) {
	q.lock.Lock()
	defer q.lock.Unlock()

	// 放入切片中，后进的元素放在数组最后面
	q.array = append(q.array, v)

	// 队中元素数量+1
	q.size = q.size + 1
}

// 出队
func (q *ArrayQueue[T]) Poll() T {
	q.lock.Lock()
	defer q.lock.Unlock()

	// 队中元素已空
	if q.size == 0 {
		panic("empty")
	}

	// 队列最前面元素
	v := q.array[0]

	// 创建新的数组，移动次数过多
	newArray := make([]T, q.size-1)
	for i := 1; i < q.size; i++ {
		copy(newArray, q.array[1:])
	}

	q.array = newArray

	// 队中元素数量-1
	q.size = q.size - 1
	return v
}

func (q *ArrayQueue[T]) Peek() T {
	return q.array[0]
}

// 栈大小
func (q *ArrayQueue[T]) Size() int {
	return q.size
}

// 栈是否为空
func (q *ArrayQueue[T]) IsEmpty() bool {
	return q.size == 0
}

var _ Queue[string] = &ArrayQueue[string]{}
