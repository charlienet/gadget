package list_test

import (
	"fmt"
	"testing"

	"github.com/charlienet/gadget/datastructures/list"
)

func TestYiled(t *testing.T) {
	l := list.NewArrayList([]string{"a", "b", "c"}...)
	for i, e := range l.Forward() {
		fmt.Println(i, ":", e)
	}

	for i, e := range l.Backward() {
		if e == "b" {
			break
		}

		fmt.Println(i, ":", e)
	}
}
