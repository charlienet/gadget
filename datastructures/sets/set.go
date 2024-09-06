package sets

import (
	"cmp"
)

type Set[T comparable] interface {
	Add(...T) Set[T]
	Remove(...T) Set[T]
	Asc() Set[T]
	Desc() Set[T]
	Contains(T) bool
	ContainsAny(...T) bool
	ContainsAll(...T) bool
	IsEmpty() bool
	ToSlice() []T // 转换为切片
}

// 并集
func Union[T cmp.Ordered](sets ...Set[T]) Set[T] {
	if len(sets) == 0 {
		return NewSet[T]()
	}
	if len(sets) == 1 {
		return sets[0]
	}

	ret := NewSet[T]()
	for i := range sets {
		ret.Add(sets[i].ToSlice()...)
	}

	return ret
}

// 交集
func Intersection[T cmp.Ordered](sets ...Set[T]) Set[T] {
	if len(sets) == 0 {
		return NewSet[T]()
	}
	if len(sets) == 1 {
		return sets[0]
	}

	ret := NewSet[T]()
	base := sets[0]
	for _, v := range base.ToSlice() {
		var insert = true
		for _, s := range sets[1:] {
			if !s.Contains(v) {
				insert = false
				break
			}
		}

		if insert {
			ret.Add(v)
		}
	}

	return ret
}

// 差集
func Difference[T cmp.Ordered](main Set[T], sets ...Set[T]) Set[T] {
	if len(sets) == 0 {
		return main
	}

	ret := NewSet[T]()
	for _, v := range sets[0].ToSlice() {
		isDiff := true
		for _, s := range sets {
			if s.Contains(v) {
				isDiff = false
			}
		}

		if isDiff {
			ret.Add(v)
		}
	}

	return ret
}
