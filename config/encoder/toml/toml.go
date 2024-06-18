package toml

import (
	"bytes"
	"github.com/charlienet/gadget/config/encoder"

	"github.com/pelletier/go-toml/v2"
)

type tomlEncoder struct{}

func (t tomlEncoder) Encode(v any) ([]byte, error) {
	b := bytes.NewBuffer(nil)
	defer b.Reset()

	err := toml.NewEncoder(b).Encode(v)
	if err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func (tomlEncoder) Decode(d []byte, v any) error {
	return toml.Unmarshal(d, v)
}

func (tomlEncoder) String() string {
	return "toml"
}

func New() encoder.Encoder {
	return tomlEncoder{}
}
