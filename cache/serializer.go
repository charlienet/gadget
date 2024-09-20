package cache

import (
	"slices"

	"github.com/charlienet/go-misc/json"
)

type Serializer interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(b []byte, v any) error
}

type jsonSerializer struct{}

func (jsonSerializer) Marshal(v any) ([]byte, error) {
	switch value := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return value, nil
	case string:
		return []byte(value), nil
	}

	return json.Marshal(v)
}

func (jsonSerializer) Unmarshal(b []byte, v any) error {
	if len(b) == 0 {
		return nil
	}

	switch value := v.(type) {
	case nil:
		return nil
	case *[]byte:
		*value = slices.Clone(b)
		return nil
	case *string:
		*value = string(b)
		return nil
	}

	return json.Unmarshal(b, &v)
}
