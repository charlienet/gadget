package redis

import "hash/fnv"

// fnv1a 是 FNV-1a 64 位哈希，供 cuckoo.go 的无模块回退实现（hashImpl）使用。
// 历史上 bitmap 布隆过滤器也用它做双哈希源；v0.5.0 起 bloom 位图哈希已换为
// xxh3-128（见 bloom.go hashs），本函数随之从 bloom.go 迁出至此独立持有。
func fnv1a(data string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(data))
	return h.Sum64()
}
