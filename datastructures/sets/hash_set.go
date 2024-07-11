package sets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/charlienet/gadget/misc/locker"
	"golang.org/x/exp/constraints"
)

type hash_set[T constraints.Ordered] struct {
	m      map[T]struct{}
	locker locker.RWLocker
}

func NewSet[T constraints.Ordered](values ...T) Set[T] {
	set := hash_set[T]{
		m: make(map[T]struct{}, len(values)),
	}

	set.Add(values...)

	return &set
}

func (s *hash_set[T]) Synchronize(values ...T) Set[T] {
	s.locker.Synchronize()
	return s
}

func (s *hash_set[T]) Add(values ...T) Set[T] {
	s.locker.Lock()
	defer s.locker.Unlock()

	for _, v := range values {
		s.m[v] = struct{}{}
	}

	return s
}

func (s *hash_set[T]) Remove(values ...T) Set[T] {
	s.locker.Lock()
	defer s.locker.Unlock()

	for _, v := range values {
		delete(s.m, v)
	}

	return s
}

func (s *hash_set[T]) Contains(value T) bool {
	s.locker.RLock()
	defer s.locker.RUnlock()

	_, ok := s.m[value]
	return ok
}

func (s *hash_set[T]) ContainsAny(values ...T) bool {
	s.locker.RLock()
	defer s.locker.RUnlock()

	for _, v := range values {
		if _, ok := s.m[v]; ok {
			return true
		}
	}

	return false
}

func (s *hash_set[T]) ContainsAll(values ...T) bool {
	s.locker.RLock()
	defer s.locker.RUnlock()

	for _, v := range values {
		if _, ok := s.m[v]; !ok {
			return false
		}
	}

	return true
}

func (s *hash_set[T]) Clone() *hash_set[T] {
	return &hash_set[T]{m: maps.Clone(s.m)}
}

func (s *hash_set[T]) Iterate(fn func(value T)) {
	for v := range s.m {
		fn(v)
	}
}

func (s *hash_set[T]) ToSlice() []T {
	values := make([]T, 0, s.Size())
	s.Iterate(func(value T) {
		values = append(values, value)
	})

	return values
}

func (s *hash_set[T]) Asc() Set[T] {
	return s.copyToSorted().Asc()
}

func (s *hash_set[T]) Desc() Set[T] {
	return s.copyToSorted().Desc()
}

func (s *hash_set[T]) copyToSorted() Set[T] {
	orderd := NewSortedSet[T]()
	for k := range s.m {
		orderd.Add(k)
	}

	return orderd
}

func (s *hash_set[T]) IsEmpty() bool {
	return len(s.m) == 0
}

func (s *hash_set[T]) Size() int {
	return len(s.m)
}

func (s *hash_set[T]) MarshalJSON() ([]byte, error) {
	items := make([]string, 0, s.Size())

	for ele := range s.m {
		b, err := json.Marshal(ele)
		if err != nil {
			return nil, err
		}

		items = append(items, string(b))
	}

	return []byte(fmt.Sprintf("[%s]", strings.Join(items, ", "))), nil
}

func (s *hash_set[T]) UnmarshalJSON(b []byte) error {
	var i []any

	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	err := d.Decode(&i)
	if err != nil {
		return err
	}

	for _, v := range i {
		if t, ok := v.(T); ok {
			s.Add(t)
		}
	}

	return nil
}

func (s *hash_set[T]) String() string {
	l := make([]string, 0, len(s.m))
	for k := range s.m {
		l = append(l, fmt.Sprint(k))
	}

	return fmt.Sprintf("{%s}", strings.Join(l, ", "))
}
