package json

import (
	"github.com/charlienet/go-misc/json"

	"github.com/charlienet/gadget/config/encoder"
)

type jsonEnoder struct{}

func New() encoder.Encoder {
	return jsonEnoder{}
}

func (jsonEnoder) Encode(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (jsonEnoder) Decode(d []byte, v any) error {
	return json.Unmarshal(d, v)
}

func (jsonEnoder) String() string { return "json" }
