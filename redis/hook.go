package redis

import (
	"context"
	"net"
	"strings"

	"github.com/redis/go-redis/v9"
)

type renameHook struct {
	prefix redisPrefix
}

func (r renameHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (r renameHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {

		// 对多个KEY进行更名操作
		for i := 0; i < len(cmds); i++ {
			r.renameKey(cmds[i])
		}

		return next(ctx, cmds)
	}
}

func (r renameHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		r.renameKey(cmd)
		return next(ctx, cmd)
	}
}

func (r renameHook) renameKey(cmd redis.Cmder) {
	if !r.prefix.hasPrefix() {
		return
	}

	args := cmd.Args()
	if len(args) == 1 {
		return
	}

	cmdName := strings.ToUpper(cmd.Name())
	switch cmdName {
	case "SELECT", "FUNCTION", "INFO":
		// 无KEY指令
	case
		"RENAME", "RENAMENX",
		"MGET",
		"RPOPLPUSH",
		"SDIFF", "SDIFFSTORE", "SINTER", "SINTERSTORE",
		"SUNION", "SUNIONSTORE",
		"WATCH":
		// 连续KEY
		r.rename(args, createSepuence(1, len(args), 1)...)
	case
		"BLPOP", "BRPOP",
		"BRPOPLPUSH ",
		"SMOVE":
		// 除最后一个外连续键
		r.rename(args, createSepuence(1, len(args)-1, 1)...)
	case "MSET", "MSETNX":
		// 间隔KEY，KEY位置规则1,3,5,7
		r.rename(args, createSepuence(1, len(args), 2)...)
	case "EVAL", "EVALSHA", "EVALSHA_RO", "FCALL", "FCALL_RO":
		// 命令中包含键数量
		if n, ok := args[2].(int); ok && n > 0 {
			r.rename(args, createSepuence(3, 3+n, 1)...)
		}
	default:
		// 默认第一个参数为键值
		r.rename(args, 1)
	}
}

func (r renameHook) rename(args []any, indexes ...int) {
	for _, i := range indexes {
		if key, ok := args[i].(string); ok {
			newKey := r.prefix.rename(key)
			args[i] = newKey
		}
	}
}

func createSepuence(start, end, step int) []int {
	ret := make([]int, 0, (end-start)/step+1)
	for i := start; i < end; i += step {
		ret = append(ret, i)
	}
	return ret
}
