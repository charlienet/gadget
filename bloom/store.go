package bloom

import "context"

type Store interface {
	Add(ctx context.Context, element string, offsets []uint64)
	Test(ctx context.Context, element string, offsets []uint64) bool
	Clear(context.Context)
	
	// AddMulti adds multiple elements to the store in batch
	AddMulti(ctx context.Context, elements []string, offsets [][]uint64)
	
	// TestMulti checks multiple elements in batch, returns []bool indicating existence
	TestMulti(ctx context.Context, elements []string, offsets [][]uint64) []bool
}
