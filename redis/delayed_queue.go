package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// QueueOption 配置延迟队列（当前无配置项，预留扩展点）。
type QueueOption func(*queueConfig)

type queueConfig struct{}

func defaultQueueConfig() queueConfig { return queueConfig{} }

// DelayedQueue 是基于 Redis ZSET 的延迟队列：
// score = 执行时间戳（UnixMilli），member = payload 字符串。
// 到期任务的取出通过单条 Lua 脚本完成（ZRANGEBYSCORE + ZREM），
// 保证并发 Dequeue 不会重复取到同一任务。
//
// 使用示例：
//
//	q := rdb.NewDelayedQueue("task:delay")
//	if err := q.Enqueue(ctx, "payload-1", time.Now().Add(time.Minute)); err != nil {
//		return err // 1 分钟后到期
//	}
//	payload, ok, err := q.Dequeue(ctx)
//	if ok {
//		// ... 处理到期任务
//	}
type DelayedQueue struct {
	client *redisClient
	key    string
}

// NewDelayedQueue 创建延迟队列（挂 *redisClient）。
func (rdb *redisClient) NewDelayedQueue(key string, opts ...QueueOption) *DelayedQueue {
	cfg := defaultQueueConfig()
	for _, o := range opts {
		o(&cfg)
	}

	return &DelayedQueue{client: rdb, key: key}
}

// Enqueue 入队一个延迟任务：payload 为 string 时直存（Dequeue 返回原样）；
// 其他类型经 json.Marshal 序列化。executeAt 为最早可执行时间。
func (q *DelayedQueue) Enqueue(ctx context.Context, payload any, executeAt time.Time) error {
	s, err := encodePayload(payload)
	if err != nil {
		return err
	}

	return q.client.ZAdd(ctx, q.key, goredis.Z{
		Score:  float64(executeAt.UnixMilli()),
		Member: s,
	}).Err()
}

// encodePayload 将 payload 序列化为字符串：string 直存，其余 json.Marshal。
func encodePayload(payload any) (string, error) {
	if s, ok := payload.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// dequeueScript 原子取一个到期任务：取 score <= now 的最早成员并 ZREM。
// 单条 Lua 脚本保证事务性：并发 Dequeue 不会重复取到同一任务。
// 无到期任务时返回 nil（RESP null → go-redis 返回 redis.Nil）。
var dequeueScript = goredis.NewScript(`
	local items = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, 1)
	if #items == 0 then
		return nil
	end
	redis.call('ZREM', KEYS[1], items[1])
	return items[1]
`)

// Dequeue 原子取出一个已到期任务。无到期任务时返回 ok=false、err=nil。
func (q *DelayedQueue) Dequeue(ctx context.Context) (string, bool, error) {
	now := time.Now().UnixMilli()
	payload, err := dequeueScript.Run(ctx, q.client, []string{q.key}, now).Result()
	if err == goredis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	s, ok := payload.(string)
	if !ok {
		return "", false, fmt.Errorf("redis: 延迟队列返回非法成员类型 %T", payload)
	}
	return s, true, nil
}

// dequeueBatchScript 原子批量取出最多 max 个到期任务并一次性 ZREM。
// 无到期任务时返回空表。
var dequeueBatchScript = goredis.NewScript(`
	local items = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, ARGV[2])
	if #items == 0 then
		return {}
	end
	redis.call('ZREM', KEYS[1], unpack(items, 1, #items))
	return items
`)

// DequeueBatch 原子批量取出最多 max 个到期任务（按 score 升序）。
// 无到期任务时返回空切片、err=nil。
func (q *DelayedQueue) DequeueBatch(ctx context.Context, max int) ([]string, error) {
	if max <= 0 {
		return nil, nil
	}

	now := time.Now().UnixMilli()
	items, err := dequeueBatchScript.Run(ctx, q.client, []string{q.key}, now, max).StringSlice()
	if err == goredis.Nil {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return items, nil
}

// PendingCount 返回队列中未到期 + 已到期但未取出的任务总数（ZCARD）。
func (q *DelayedQueue) PendingCount(ctx context.Context) (int64, error) {
	return q.client.ZCard(ctx, q.key).Result()
}

// 设计决策：延迟队列不做失效兜底策略（fail-open/fail-closed）。
// 队列是持久化状态（ZSET 成员），服务不可用时 Enqueue 无法安全降级
// （放行=任务丢失、拒绝=任务丢弃），Dequeue 兜底会重复消费或丢任务，
// 因此直接返回原始错误，由调用方决定重试。与限流/过滤器等无状态保护性
// 能力不同，队列兜底会产生数据一致性问题。
