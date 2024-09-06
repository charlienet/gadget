package sets

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/charlienet/gadget/misc/locker"
)

type sorted_set[T cmp.Ordered] struct {
	sorted []T
	set    Set[T]
	locker locker.RWLocker
}

func NewSortedSet[T cmp.Ordered](t ...T) *sorted_set[T] {
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

func (s *sorted_set[T]) MarshalJSON() ([]byte, error) {
	items := make([]string, 0, len(s.sorted))

	for _, ele := range s.sorted {
		b, err := json.Marshal(ele)
		if err != nil {
			return nil, err
		}

		items = append(items, string(b))
	}

	return []byte(fmt.Sprintf("[%s]", strings.Join(items, ", "))), nil
}
