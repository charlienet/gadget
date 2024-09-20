package cache

import (
	"testing"

	"github.com/charlienet/go-misc/json"
	"github.com/stretchr/testify/assert"
)

type User struct {
	Name string
	Role Role
}

type Role struct {
	Name string
}

var u = User{Name: "test", Role: Role{Name: "admin"}}

func TestJsonSerialize(t *testing.T) {
	s := &jsonSerializer{}
	b, err := s.Marshal(u)
	assert.Nil(t, err)

	t.Log(string(b))

	var un User
	assert.Nil(t, s.Unmarshal(b, &un))

	t.Log(json.Struct2Json(un))

	b2, _ := s.Marshal("abc")
	t.Log(string(b2))

	var r string
	s.Unmarshal(b2, &r)
	t.Log(r)
}

func BenchmarkMarshal(b *testing.B) {
	b.Run("j", func(b *testing.B) {
		v := "abc"
		for i := 0; i < b.N; i++ {
			j := &jsonSerializer{}
			j.Marshal(v)
		}
	})

	b.Run("j", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			j := &jsonSerializer{}
			j.Marshal(u)
		}
	})

}
