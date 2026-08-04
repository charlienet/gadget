package redis

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/charlienet/gadget/redis"
)

const (
	keys_name = "KEYS"
)

type options struct {
	rdb redis.Client
	key string
}

type keys_struct struct {
	Keys []uint64
}

func (o options) getSetKeys(ctx context.Context, keys []uint64) []uint64 {
	key := fmt.Sprintf("%s_%s", o.key, keys_name)

	s := keys_struct{Keys: keys}
	b, _ := json.Marshal(s)
	exist, _ := o.rdb.SetNX(ctx, key, b, 0).Result()
	if exist {
		ret, _ := o.rdb.Get(ctx, key).Bytes()
		_ = json.Unmarshal(ret, &s)

		return s.Keys
	}

	return keys
}
