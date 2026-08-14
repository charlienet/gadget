package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/go-version"
)

// Constraint 是实例约束函数：对 client 进行校验，返回非 nil 错误表示约束不满足。
// 通过 Client.Constraint/MustConstraint 执行，可自定义扩展。
type Constraint func(Client) error

func Ping() Constraint {
	return func(rc Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
		defer cancel()

		return rc.Ping(ctx).Err()
	}
}

func Version(expended string) Constraint {
	return func(rc Client) error {
		v := rc.ServerVersion()
		if len(v) == 0 {
			return errors.New("version not obtained")
		}
		current, err := version.NewVersion(v)
		if err != nil {
			return err
		}

		constraint, err := version.NewConstraint(expended)
		if err != nil {
			return err
		}

		if !constraint.Check(current) {
			return fmt.Errorf("the desired version is %v, which does not match the expected version %v", current, expended)
		}

		return nil
	}
}
