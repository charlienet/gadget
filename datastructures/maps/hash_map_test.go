package maps_test

import (
	"encoding/json"
	"log"
	"testing"

	"github.com/charlienet/gadget/datastructures/maps"
)

func TestNew(t *testing.T) {

	a := maps.NewHashMap(map[string]string{"ccc": "DD"}, map[string]string{"97": "sd"}, map[string]string{"aa": "cdcdc"})

	a.Set("sbc", "abc")
	t.Log(a.Get("sbc"))
	t.Log(a.Get("ccc"))
	t.Log(a.Get("97"))
	t.Log(a.Get("aaa"))

	a.Synchronize()

	for n := range a.Keys() {
		println(n)
	}

	for k, v := range a.Each() {
		println(k, "=", v)
	}

	dc, err := json.MarshalIndent(a, " ", " ")
	if err != nil {
		t.Error(err)
	}
	

	println("json:", string(dc))

	m := maps.Collect[map[string]string](a)
	_ = m

	log.Println("sss:", m)

	_ = a
}
