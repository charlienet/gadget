package maps

import "iter"

type Map[M ~map[K]V, K comparable, V any] interface {
	Each() iter.Seq2[K, V]
}

func Collect[M ~map[K]V, K comparable, V any](ms ...Map[M, K, V]) map[K]V {
	m := make(map[K]V)

	for _, mm := range ms {
		for k, v := range mm.Each() {
			m[k] = v
		}
	}

	return m
}
