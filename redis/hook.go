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
		"DBSIZE",
		"ECHO",
		"FAILOVER", "FLUSHALL", "FLUSHDB", "FUNCTION",
		"HELLO",
		"INFO",
		"LASTSAVE", "LATENCY", "LOLWUT",
		"MODULE", "MONITOR",
		"PING", "POST", "PSYNC",
		"RANDOMKEY", "READONLY", "READWRITE", "REPLCONF", "REPLICAOF", "ROLE",
		"SAVE", "SCAN", "SCRIPT", "SELECT", "SHUTDOWN", "SLAVEOF", "SLOWLOG",
		"SUBSCRIBE", "PSUBSCRIBE", "SSUBSCRIBE", "SWAPDB", "SYNC",
		"TIME",
		"UNSUBSCRIBE", "PUNSUBSCRIBE", "SUNSUBSCRIBE",
		"WAIT", "WAITAOF",
		// TimeSeries: 无key
		"TS.QUERYINDEX", "TS.MRANGE", "TS.MREVRANGE", "TS.MGET",
		"TS.QUERYLABELS":
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
		"SUNIONCARD", "SDIFFCARD",
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
	// LMOVEM src dest LEFT|RIGHT... element [element...], BLMOVEM ... timeout
	// TS.CREATERULE sourceKey destKey [AGGREGATE...]
	// TS.DELETERULE sourceKey destKey
	case "COPY", "LCS", "LMOVE", "BLMOVE", "LMOVEM", "BLMOVEM",
		"ZRANGESTORE", "GEOSEARCHSTORE",
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

	// 模式I: MIGRATE — args[3]为key，如果包含KEYS参数则处理后续的keys
	// MIGRATE host port key|"" db timeout [COPY] [REPLACE] [KEYS key...]
	case "MIGRATE":
		if len(args) >= 4 {
			// 如果key参数不为空字符串，则重命名
			if key, ok := args[3].(string); !ok || key != "" {
				r.rename(args, 3)
			}
			
			// 处理 [KEYS key...] 参数
			for i := 4; i < len(args); i++ {
				if str, ok := args[i].(string); ok && strings.ToUpper(str) == "KEYS" && i+1 < len(args) {
					// 从KEYS之后的所有参数都是key
					for j := i + 1; j < len(args); j++ {
						r.rename(args, j)
					}
					break
				}
			}
		}

	// 模式J: 特殊命令处理
	case "XREAD", "XREADGROUP":
		// XREAD [COUNT count] [BLOCK ms] [MAXCOUNT count] [MAXSIZE size] STREAMS key [key...] id [id...]
		// XREADGROUP GROUP group consumer [COUNT count] [BLOCK ms] [NOACK] [MAXCOUNT count] [MAXSIZE size] STREAMS key [key...] id [id...]
		// 找到 STREAMS 关键字的位置，然后重命名其后的 key（前半部分是 key，后半部分是 id）
		streamsIndex := -1
		for i, arg := range args {
			if str, ok := arg.(string); ok && strings.ToUpper(str) == "STREAMS" {
				streamsIndex = i
				break
			}
		}
		if streamsIndex != -1 {
			// 计算有多少个 key（等于 id 的数量）
			remainingArgs := len(args) - streamsIndex - 1
			keyCount := remainingArgs / 2 // key 和 id 成对出现
			if keyCount > 0 {
				r.rename(args, createSepuence(streamsIndex+1, streamsIndex+1+keyCount, 1)...)
			}
		}

	case "HIMPORT":
		// HIMPORT key field value [field value...]
		// args[1] 是 key，后面是 field/value 对
		r.rename(args, 1)

	case "TS.READ":
		// TS.READ key [LATEST] [FROM_TIMESTAMP] [TO_TIMESTAMP] [FILTER_BY_TS ts...] [FILTER_BY_VALUE min max] [COUNT count] [[ALIGN align] AGGREGATION aggregator bucketDuration [BUCKETTIMESTAMP bt] [EMPTY]]
		// args[1] 是 key
		r.rename(args, 1)

	case "TS.NRANGE", "TS.NREVRANGE":
		// TS.NRANGE key [LATEST] FROM_TIMESTAMP TO_TIMESTAMP [FILTER_BY_TS ts...] [FILTER_BY_VALUE min max] [COUNT count] [[ALIGN align] AGGREGATION aggregator bucketDuration [BUCKETTIMESTAMP bt] [EMPTY]]
		// args[1] 是 key
		r.rename(args, 1)

	case "SORT":
		// SORT key [BY pattern] [LIMIT offset count] [GET pattern [GET pattern ...]] [ASC|DESC] [ALPHA] [STORE dest]
		// args[1] 是 key（已被 default 处理），还需要处理 STORE 后面的 dest key
		r.rename(args, 1) // 处理主 key
		storeIndex := -1
		for i, arg := range args {
			if str, ok := arg.(string); ok && strings.ToUpper(str) == "STORE" {
				storeIndex = i
				break
			}
		}
		if storeIndex != -1 && storeIndex+1 < len(args) {
			r.rename(args, storeIndex+1) // 重命名 STORE 后的 dest key
		}

	case "GEORADIUS", "GEORADIUSBYMEMBER":
		// GEORADIUS key lon lat radius unit [WITHCOORD] [WITHDIST] [WITHHASH] [COUNT count] [ASC|DESC] [STORE key] [STOREDIST key]
		// GEORADIUSBYMEMBER key member radius unit [STORE key] [STOREDIST key]
		// args[1] 是 key（已被 default 处理），还需要处理 STORE 和 STOREDIST 后面的 key
		r.rename(args, 1) // 处理主 key
		for i := 2; i < len(args)-1; i++ {
			if str, ok := args[i].(string); ok {
				upperStr := strings.ToUpper(str)
				if upperStr == "STORE" || upperStr == "STOREDIST" {
					r.rename(args, i+1) // 重命名 STORE 或 STOREDIST 后的 key
				}
			}
		}

	case "DEBUG":
		// DEBUG OBJECT key
		// 当 args[1] 是 "OBJECT" 时，重命名 args[2]
		if len(args) >= 3 {
			if str, ok := args[1].(string); ok && strings.ToUpper(str) == "OBJECT" {
				r.rename(args, 2)
			}
		}

	case "PUBSUB":
		// PUBSUB CHANNELS [pattern] / PUBSUB NUMSUB [channel [channel ...]] / PUBSUB NUMPAT
		// channel/pattern 不是 key，不需要添加前缀
		// 此分支保留以明确处理该命令，但不执行任何重命名操作
		return

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
