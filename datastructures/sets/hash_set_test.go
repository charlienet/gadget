package sets_test

import (
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
}
