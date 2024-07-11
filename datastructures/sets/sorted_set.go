package sets

import (
	"slices"

	"github.com/charlienet/gadget/misc/locker"
	"golang.org/x/exp/constraints"
)

type sorted_set[T constraints.Ordered] struct {
	sorted []T
	set    Set[T]
	locker locker.RWLocker
}

func NewSortedSet[T constraints.Ordered](t ...T) *sorted_set[T] {
	return &sorted_set[T]{
		sorted: t,
		set:    NewSet(t...),
	}
}

func (s *sorted_set[T]) Synchronize(values ...T) Set[T] {
	s.locker.Synchronize()
	return s
}

func (s *sorted_set[T]) Add(values ...T) Set[T] {
	s.locker.Lock()
	defer s.locker.Unlock()

	for _, v := range values {
		if !s.set.Contains(v) {
			s.sorted = append(s.sorted, v)
			s.set.Add(v)
		}
	}

	return s
}

func (s *sorted_set[T]) Remove(values ...T) Set[T] {
	s.locker.Lock()
	defer s.locker.Unlock()

	for _, v := range values {
		if s.set.Contains(v) {
			for index := range s.sorted {
				if s.sorted[index] == v {
					s.sorted = append(s.sorted[:index], s.sorted[index+1:]...)
					break
				}
			}

			s.set.Remove(v)
		}
	}

	return s
}

func (s *sorted_set[T]) Asc() Set[T] {
	s.locker.Lock()
	defer s.locker.Unlock()

	keys := s.sorted
	slices.Sort(keys)

	return &sorted_set[T]{
		sorted: keys,
		set:    NewSet(keys...),
	}
}

func (s *sorted_set[T]) Desc() Set[T] {
	s.locker.Lock()
	defer s.locker.Unlock()

	keys := s.sorted
	slices.SortFunc(keys, func(a, b T) int {
		if a == b {
			return 0
		}

		if a > b {
			return -1
		} else {
			return 1
		}
	})

	return &sorted_set[T]{
		sorted: keys,
		set:    NewSet(keys...),
	}
}

func (s *sorted_set[T]) Contains(v T) bool {
	s.locker.RLock()
	defer s.locker.RUnlock()

	return s.set.Contains(v)
}

func (s *sorted_set[T]) ContainsAny(values ...T) bool {
	s.locker.RLock()
	defer s.locker.RUnlock()

	return s.set.ContainsAny(values...)
}

func (s *sorted_set[T]) ContainsAll(values ...T) bool {
	s.locker.RLock()
	defer s.locker.RUnlock()

	return s.set.ContainsAll(values...)
}

func (s *sorted_set[T]) IsEmpty() bool {
	s.locker.RLock()
	defer s.locker.RUnlock()

	return s.set.IsEmpty()
}

func (s *sorted_set[T]) ToSlice() []T {
	s.locker.RLock()
	defer s.locker.RUnlock()

	return s.sorted
}
