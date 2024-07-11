package cache

import (
	"encoding/json"
)

type serializer struct {
}

func (serializer) Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (serializer) Unmarshal(b []byte, v any) error {
	return json.Unmarshal(b, &v)
}
