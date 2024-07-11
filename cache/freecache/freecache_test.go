package freecache

import (
	"context"
	"testing"
)

func TestGetSet(t *testing.T) {
	c := New()
	t.Log(c.Get(context.TODO(), "abc"))

	v := []byte("abc")
	c.Set(context.Background(), "abc", v, 20)

	ret, exist, err := c.Get(context.Background(), "abc")
	t.Log(string(ret), exist, err)
}
