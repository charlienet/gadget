package sets_test

import (
	"encoding/json"
	"testing"

	"github.com/charlienet/gadget/datastructures/sets"
	"github.com/stretchr/testify/require"
)

func TestHashSet(t *testing.T) {
	s := sets.NewSet("a", "b", "c")
	s.Add("d", "e")

	require.True(t, s.Contains("b"))
	require.True(t, s.ContainsAny("b", "f", "g"))
	require.False(t, s.ContainsAll("b", "f", "g"))

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(b))

	_ = min(1, 3)
}

func TestMarshal(t *testing.T) {
	s := sets.NewSortedSet("b", "c", "a")
	s.Asc()

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(b))
}
