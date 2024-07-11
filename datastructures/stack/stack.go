package stack

import "sync"

var _ Stack[string] = &ArrayStack[string]{}

type Stack[T any] interface {
	Push(T)
	Pop() T
}

type ArrayStack[T any] struct {
	array []T        // 底层切片
	size  int        // 栈的元素数量
	lock  sync.Mutex // 为了并发安全使用的锁
}

// 初始化堆栈
func NewArrayStack[T any]() *ArrayStack[T] {
	return &ArrayStack[T]{}
}

// 入栈
func (s *ArrayStack[T]) Push(v T) {
	s.lock.Lock()
	defer s.lock.Unlock()

	// 放入切片中，后进的元素放在数组最后面
	s.array = append(s.array, v)

	// 栈中元素数量+1
	s.size = s.size + 1
}

func (s *ArrayStack[T]) Pop() T {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.size == 0 {
		panic("empty")
	}

	// 栈顶元素
	v := s.array[s.size-1]

	// 切片收缩，但可能占用空间越来越大
	//stack.array = stack.array[0 : stack.size-1]

	// 创建新的数组，空间占用不会越来越大，但可能移动元素次数过多
	newArray := make([]T, s.size-1)
	for i := 0; i < s.size-1; i++ {
		newArray[i] = s.array[i]
	}
	s.array = newArray

	// 栈中元素数量-1
	s.size = s.size - 1
	return v
}

// 获取栈顶元素
func (s *ArrayStack[T]) Peek() T {
	// 栈中元素已空
	if s.size == 0 {
		panic("empty")
	}

	// 栈顶元素值
	v := s.array[s.size-1]
	return v
}

// 栈大小
func (s *ArrayStack[T]) Size() int {
	return s.size
}

// 栈是否为空
func (s *ArrayStack[T]) IsEmpty() bool {
	return s.size == 0
}
