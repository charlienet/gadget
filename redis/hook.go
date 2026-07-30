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

	// --- 模块级命令排除（整个模块无key或不用标准key位置）---

	// FT.* (RediSearch) — args[1]均为索引名/别名/子命令，非key
	if strings.HasPrefix(cmdName, "FT.") {
		return
	}

	// TFUNCTION — 子命令非key（TFUNCTION LOAD/DELETE/LIST）
	if strings.HasPrefix(cmdName, "TFUNCTION") {
		return
	}

	// 模式A: 无KEY指令 — 所有参数都不是key
	switch cmdName {
	case
		"AUTH", "BGREWRITEAOF", "BGSAVE",
		"CLIENT", "CLUSTER", "COMMAND", "CONFIG",
		"DBSIZE", "DEBUG",
		"ECHO",
		"FAILOVER", "FLUSHALL", "FLUSHDB", "FUNCTION",
		"HELLO",
		"INFO",
		"LASTSAVE", "LATENCY", "LOLWUT",
		"MODULE", "MONITOR",
		"PING", "POST", "PSYNC", "PUBSUB",
		"RANDOMKEY", "READONLY", "READWRITE", "REPLCONF", "REPLICAOF", "ROLE",
		"SAVE", "SCAN", "SCRIPT", "SELECT", "SHUTDOWN", "SLAVEOF", "SLOWLOG",
		"SUBSCRIBE", "PSUBSCRIBE", "SSUBSCRIBE", "SWAPDB", "SYNC",
		"TIME",
		"UNSUBSCRIBE", "PUNSUBSCRIBE", "SUNSUBSCRIBE",
		"WAIT", "WAITAOF",
		// TimeSeries: 无key
		"TS.QUERYINDEX", "TS.MRANGE", "TS.MREVRANGE", "TS.MGET":
		return
	}

	// 按参数模式匹配key位置
	switch cmdName {

	// 模式B: 连续KEY — 从args[1]到末尾全部是key
	case
		"DEL", "EXISTS",
		"MGET",
		"PFCOUNT", "PFMERGE",
		"RENAME", "RENAMENX", "RPOPLPUSH",
		"SDIFF", "SDIFFSTORE", "SINTER", "SINTERSTORE",
		"SUNION", "SUNIONSTORE",
		"TOUCH",
		"UNLINK",
		"WATCH":
		r.rename(args, createSepuence(1, len(args), 1)...)

	// 模式C: 除最后一个外连续KEY — 末尾参数不是key（如timeout）
	case
		"BLPOP", "BRPOP",
		"BRPOPLPUSH",
		"BZPOPMAX", "BZPOPMIN",
		"SMOVE",
		// JSON.MGET key [key...] path — 末尾path非key
		"JSON.MGET":
		r.rename(args, createSepuence(1, len(args)-1, 1)...)

	// 模式D: 间隔KEY — key/value交替，key在奇数位置(1,3,5...)
	case "MSET", "MSETNX":
		r.rename(args, createSepuence(1, len(args), 2)...)

	// 模式D2: key/非key/非key 间隔 — key在1,4,7...
	// JSON.MSET key path value [key path value...]
	// TS.MADD key ts val [key ts val...]
	case "JSON.MSET", "TS.MADD":
		r.rename(args, createSepuence(1, len(args), 3)...)

	// 模式E1: 脚本类 — 通过args[2]的count指定key数量
	// EVAL script numkeys key [key...] arg [arg...]
	// FCALL func numkeys key [key...] arg [arg...]
	// TFCALL lib.func numkeys key [key...] arg [arg...]
	case "EVAL", "EVALSHA", "EVAL_RO", "EVALSHA_RO",
		"FCALL", "FCALL_RO",
		"TFCALL", "TFCALLASYNC":
		if n, ok := args[2].(int); ok && n > 0 {
			r.rename(args, createSepuence(3, 3+n, 1)...)
		}

	// 模式E2: 聚合写命令 — args[1]为dest key，args[2]为count，args[3..3+n]为source keys
	// ZINTERSTORE dest numkeys key [key...] [WEIGHTS...]
	// CMS.MERGE/TDIGEST.MERGE dest numkeys key [key...]
	case "ZINTERSTORE", "ZUNIONSTORE", "ZDIFFSTORE",
		"CMS.MERGE", "TDIGEST.MERGE":
		if len(args) >= 3 {
			r.rename(args, 1) // destination key
			if n, ok := args[2].(int); ok && n > 0 {
				r.rename(args, createSepuence(3, 3+n, 1)...)
			}
		}

	// 模式E3: 只读聚合 — args[1]为count，keys从args[2]开始
	// ZINTER numkeys key [key...] [WEIGHTS...]
	case
		"ZINTER", "ZUNION", "ZDIFF", "ZINTERCARD", "SINTERCARD",
		"LMPOP", "BLMPOP", "ZMPOP", "BZMPOP":
		if len(args) >= 2 {
			if n, ok := args[1].(int); ok && n > 0 {
				r.rename(args, createSepuence(2, 2+n, 1)...)
			}
		}

	// 模式F: 子命令后有key — args[2]为key
	// OBJECT ENCODING key / MEMORY USAGE key / DEBUG OBJECT key
	case "OBJECT":
		if len(args) >= 3 {
			r.rename(args, 2)
		}
	case "MEMORY":
		if len(args) >= 3 {
			r.rename(args, 2)
		}

	// 模式G: 固定双key — args[1]和args[2]都是key，后面可能有选项参数
	// COPY src dest [DB...], LCS key1 key2 [LEN...]
	// LMOVE src dest LEFT|RIGHT..., BLMOVE src dest LEFT|RIGHT... timeout
	// TS.CREATERULE sourceKey destKey [AGGREGATE...]
	// TS.DELETERULE sourceKey destKey
	case "COPY", "LCS", "LMOVE", "BLMOVE", "ZRANGESTORE", "GEOSEARCHSTORE",
		"TS.CREATERULE", "TS.DELETERULE":
		if len(args) >= 3 {
			r.rename(args, 1, 2)
		}

	// 模式H: BITOP — args[2]为dest key，args[3..]为source keys
	// BITOP operation destkey key [key...]
	case "BITOP":
		if len(args) >= 3 {
			r.rename(args, createSepuence(2, len(args), 1)...)
		}

	// 模式I: MIGRATE — args[3]为key
	// MIGRATE host port key|"" db timeout [COPY] [REPLACE] [KEYS key...]
	case "MIGRATE":
		if len(args) >= 4 {
			r.rename(args, 3)
		}

	default:
		// 默认模式: 第一个参数为键值（覆盖90%+的命令）
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
