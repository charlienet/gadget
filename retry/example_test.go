package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ExampleDo 演示固定退避 + 次数上限的基本用法。
func ExampleDo() {
	attempts := 0
	err := Do(context.Background(),
		func(ctx context.Context) error {
			attempts++
			return errors.New("connection refused")
		},
		WithBackoff(Fixed(1*time.Millisecond)),
		WithMaxAttempts(3),
	)
	fmt.Printf("attempts=%d err=%v\n", attempts, err)
	// Output:
	// attempts=3 err=connection refused
}
