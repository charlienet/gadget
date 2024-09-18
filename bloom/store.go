package bloom

import "context"

type Store interface {
	Add(ctx context.Context, element string, offsets []uint64)
	Test(ctx context.Context, element string, offsets []uint64) bool
	Clear(context.Context)
}
