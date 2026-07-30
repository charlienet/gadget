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

func TestSerializerEdgeCases(t *testing.T) {
	s := &jsonSerializer{}

	// nil value marshaling
	data, err := s.Marshal(nil)
	assert.Nil(t, err)
	assert.Nil(t, data)

	// []byte passthrough
	original := []byte("rawbytes")
	data, err = s.Marshal(original)
	assert.Nil(t, err)
	assert.Equal(t, original, data)

	// string passthrough
	data, err = s.Marshal("hello")
	assert.Nil(t, err)
	assert.Equal(t, []byte("hello"), data)

	// Unmarshal nil
	err = s.Unmarshal(nil, nil)
	assert.Nil(t, err)

	// Unmarshal empty
	err = s.Unmarshal([]byte{}, nil)
	assert.Nil(t, err)

	// Unmarshal to *[]byte
	var b []byte
	err = s.Unmarshal([]byte("bytes"), &b)
	assert.Nil(t, err)
	assert.Equal(t, []byte("bytes"), b)

	// Unmarshal to *string
	var str string
	err = s.Unmarshal([]byte("hello"), &str)
	assert.Nil(t, err)
	assert.Equal(t, "hello", str)
}
