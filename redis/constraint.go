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

// Ping 实例约束：启动期校验连通性。
//
// Background 豁免：约束在实例接入业务流量前执行，此刻不存在调用方请求
// 上下文可传递；探测被固定 3s deadline 收窄、不向下游传播。本库"无 ctx
// 公开 API 内刻意使用 Background"的决策记录见 README v0.9.0 节。
func Ping() Constraint {
	return func(rc Client) error {
		// 探测被固定 3s deadline 收窄、不向下游传播。
		// 豁免：接口契约（启动期约束检查）无 ctx 入口，此处为背景决策记录（README v0.9.0 同级）。
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
		defer cancel()

		return rc.Ping(ctx).Err()
	}
}

// Version 实例约束：校验服务器版本满足给定约束表达式（如 ">=7.0"，
// hashicorp/go-version 语法）。版本读自 Capability 缓存（ServerVersion），
// 未显式 Capability().Probe(ctx) 时为空串、约束判为失败；版本不可解析时
// 返回解析错误。
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
			return fmt.Errorf("redis: server version %v does not match expected constraint %q", current, expended)
		}

		return nil
	}
}
