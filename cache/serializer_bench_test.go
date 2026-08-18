package cache

import (
	"encoding/json"
	"testing"

	"github.com/bytedance/sonic"
)

type benchUser struct {
	ID   int64
	Name string
	Tags []string
}

var benchData = benchUser{
	ID:   42,
	Name: "bench-user",
	Tags: []string{"a", "b", "c"},
}

// BenchmarkSerializerMarshal 对比标准库 encoding/json 与 sonic（默认序列化器）的序列化性能。
func BenchmarkSerializerMarshal(b *testing.B) {
	b.Run("std-json", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = json.Marshal(benchData)
		}
	})
	b.Run("sonic", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = sonic.ConfigStd.Marshal(benchData)
		}
	})
}

// BenchmarkSerializerUnmarshal 对比标准库 encoding/json 与 sonic 的反序列化性能。
func BenchmarkSerializerUnmarshal(b *testing.B) {
	raw, _ := json.Marshal(benchData)

	b.Run("std-json", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var u benchUser
			_ = json.Unmarshal(raw, &u)
		}
	})
	b.Run("sonic", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var u benchUser
			_ = sonic.ConfigStd.Unmarshal(raw, &u)
		}
	})
}
