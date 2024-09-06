package maps

import (
	"encoding/json"
	"iter"
	"maps"

	"github.com/charlienet/gadget/misc/locker"
)

type hashmap[M ~map[K]V, K comparable, V any] struct {
	m    M
	l    locker.RWLocker
	sync bool
}

func NewHashMap[M ~map[K]V, K comparable, V any](mm ...M) *hashmap[M, K, V] {
	m := make(M)
	for _, v := range mm {
		maps.Copy(m, v)
	}

	return &hashmap[M, K, V]{
		m: m,
	}
}

func (h *hashmap[M, K, V]) Synchronize() *hashmap[M, K, V] {
	h.l.Synchronize()
	h.sync = true

	return h
}

func (h *hashmap[M, K, V]) Set(k K, v V) {
	h.l.Lock()
	h.m[k] = v
	h.l.Unlock()
}

func (h *hashmap[M, K, V]) Get(key K) (V, bool) {
	h.l.RLock()
	defer h.l.RUnlock()

	v, ok := h.m[key]
	return v, ok
}

func (h *hashmap[M, K, V]) Delete(k K) {
	h.l.Lock()
	delete(h.m, k)
	h.l.Unlock()
}

func (h *hashmap[M, K, V]) DeleteFunc(del func(K, V) bool) {
	for k, v := range h.m {
		if del(k, v) {
			delete(h.m, k)
		}
	}
}

func (h *hashmap[M, K, V]) Clear() *hashmap[M, K, V] {
	clear(h.m)
	return h
}

func (h *hashmap[M, K, V]) Clone() *hashmap[M, K, V] {
	c := maps.Clone(h.m)
	r := &hashmap[M, K, V]{
		m: c,
	}

	if h.sync {
		r.Synchronize()
	}

	return r
}

func (h *hashmap[M, K, V]) Each() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		h.l.RLock()
		defer h.l.RUnlock()

		for k, v := range h.m {
			if !yield(k, v) {
				return
			}
		}
	}
}

func (h *hashmap[M, K, V]) Keys() iter.Seq[K] {
	return func(yield func(K) bool) {
		h.l.RLock()
		defer h.l.RUnlock()

		for k := range h.m {
			if !yield(k) {
				return
			}
		}
	}
}

func (h *hashmap[M, K, V]) Values() iter.Seq[V] {
	return func(yield func(V) bool) {
		h.l.RLock()
		defer h.l.RUnlock()

		for _, v := range h.m {
			if !yield(v) {
				return
			}
		}
	}
}

func (h *hashmap[M, K, V]) MarshalJSON() ([]byte, error) {
	return json.Marshal(h.m)
}
