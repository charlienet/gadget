package queue

import "fmt"

type CircleQueue[T any] struct {
	data  []any
	cap   int
	front int
	rear  int
}

func NewCircleQueue[T any](cap int) *CircleQueue[T] {
	cap++

	return &CircleQueue[T]{
		data: make([]any, cap),
		cap:  cap,
	}
}

func (q *CircleQueue[T]) Push(data any) bool {
	if (q.rear+1)%q.cap == q.front {
		return false
	}

	q.data[q.rear] = data
	q.rear = (q.rear + 1) % q.cap
	return true
}
func (q *CircleQueue[T]) Pop() any {
	if q.rear == q.front {
		return nil
	}

	data := q.data[q.front]
	q.data[q.front] = nil
	q.front = (q.front + 1) % q.cap
	return data
}

func (q *CircleQueue[T]) Size() int {
	return (q.rear - q.front + q.cap) % q.cap
}

func (q *CircleQueue[T]) IsFull() bool {
	return (q.rear+1)%q.cap == q.front
}

func (q *CircleQueue[T]) IsEmpty() bool {
	return q.front == q.rear
}

func (q *CircleQueue[T]) Show() string {
	return fmt.Sprintf("%v", q.data)
}
