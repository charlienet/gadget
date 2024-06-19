package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/go-version"
)

type constraintFunc func(redisClient) error

func Ping() constraintFunc {
	return func(rc redisClient) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
		defer cancel()

		return rc.Ping(ctx).Err()
	}
}

func Version(expended string) constraintFunc {
	return func(rc redisClient) error {
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
