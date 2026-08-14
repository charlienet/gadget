package redis

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

// casNil 哨兵值：标记"期望 key 不存在"的 CAS 语义（对应 SETNX）。
// 选用含特殊前缀的字符串，与业务值冲突概率极低。
const casNil = "__GADGET_CAS_NIL__"

// casSetScript 原子比较并设置：
//   - 期望值等于 casNil：仅当 key 不存在时设置（EXISTS 判断，SETNX 语义）
//   - 否则：仅当 key 当前值等于期望值时设置（GET 比较）
//
// 返回 1 表示比较成立且已设置，0 表示值不匹配（未修改）。
var casSetScript = goredis.NewScript(`
	if ARGV[1] == '__GADGET_CAS_NIL__' then
		if redis.call('EXISTS', KEYS[1]) == 1 then
			return 0
		end
		redis.call('SET', KEYS[1], ARGV[2])
		return 1
	end
	if redis.call('GET', KEYS[1]) == ARGV[1] then
		redis.call('SET', KEYS[1], ARGV[2])
		return 1
	end
	return 0
`)

// CompareAndSet 原子比较并设置：key 当前值等于 oldValue 时设置为 newValue。
// 值按字符串比较（Redis 存储即字符串；number/bool 等经 %v 转为字符串后比较）。
// 传 oldValue=nil 表示"仅当 key 不存在时设置"（SETNX 语义）。
// 返回 true 表示比较成立且已设置；false 表示值不匹配（未修改）。
func (rdb *redisClient) CompareAndSet(ctx context.Context, key string, oldValue, newValue any) (bool, error) {
	old := fmt.Sprintf("%v", oldValue)
	if oldValue == nil {
		old = casNil
	}

	n, err := casSetScript.Run(ctx, rdb, []string{key}, old, fmt.Sprintf("%v", newValue)).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// casDeleteScript 原子比较并删除：
//   - 期望值等于 casNil：key 存在即删除（无条件删除，与 DEL 等价）
//   - 否则：仅当 key 当前值等于期望值时删除
//
// 返回 1 表示已删除，0 表示值不匹配（未删除）。
var casDeleteScript = goredis.NewScript(`
	if ARGV[1] == '__GADGET_CAS_NIL__' then
		return redis.call('DEL', KEYS[1])
	end
	if redis.call('GET', KEYS[1]) == ARGV[1] then
		return redis.call('DEL', KEYS[1])
	end
	return 0
`)

// CompareAndDelete 原子比较并删除：key 当前值等于 oldValue 时删除。
// 传 oldValue=nil 表示"key 存在即删除"（无条件删除）。
// 返回 true 表示已删除；false 表示值不匹配（未删除）。
func (rdb *redisClient) CompareAndDelete(ctx context.Context, key string, oldValue any) (bool, error) {
	old := fmt.Sprintf("%v", oldValue)
	if oldValue == nil {
		old = casNil
	}

	n, err := casDeleteScript.Run(ctx, rdb, []string{key}, old).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
