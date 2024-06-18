package yaml

import (
	"github.com/charlienet/gadget/config/encoder"

	"gopkg.in/yaml.v3"
)

type yamlEncoder struct{}

func (yamlEncoder) Encode(v any) ([]byte, error) {
	return yaml.Marshal(v)
}

func (yamlEncoder) Decode(b []byte, v any) error {
	return yaml.Unmarshal(b, &v)
}

func (yamlEncoder) String() string { return "yaml" }

func New() encoder.Encoder {
	return yamlEncoder{}
}
