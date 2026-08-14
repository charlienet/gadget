package redis_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/charlienet/gadget/redis"
	"github.com/charlienet/gadget/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDelayedQueue 验证延迟队列（ZSET + Lua 原子取出）。
func TestDelayedQueue(t *testing.T) {
	test.RunOnMiniRedis(t, func(rdb redis.Client) {
		ctx := context.Background()
		q := rdb.NewDelayedQueue("dq:1")

		t.Run("未到期不可取", func(t *testing.T) {
			require.NoError(t, rdb.Del(ctx, "dq:1").Err())
			require.NoError(t, q.Enqueue(ctx, "future", time.Now().Add(time.Hour)))

			payload, ok, err := q.Dequeue(ctx)
			require.NoError(t, err)
			assert.False(t, ok, "未到期任务不应被取出")
			assert.Empty(t, payload)
		})

		t.Run("到期可取且取后消失", func(t *testing.T) {
			require.NoError(t, q.Enqueue(ctx, "ready", time.Now().Add(-time.Second)))

			payload, ok, err := q.Dequeue(ctx)
			require.NoError(t, err)
			assert.True(t, ok, "到期任务应可取")
			assert.Equal(t, "ready", payload)

			// 已取出：再取应为空
			_, ok, err = q.Dequeue(ctx)
			require.NoError(t, err)
			assert.False(t, ok, "任务被取走后不应再被取出")
		})

		t.Run("原子性：同一任务只被取一次", func(t *testing.T) {
			require.NoError(t, q.Enqueue(ctx, "solo", time.Now().Add(-time.Second)))

			p1, ok1, err := q.Dequeue(ctx)
			require.NoError(t, err)
			assert.True(t, ok1)

			p2, ok2, err := q.Dequeue(ctx)
			require.NoError(t, err)
			assert.False(t, ok2, "单线程顺序模拟并发：任务被取走后第二次 Dequeue 应为空")
			assert.Equal(t, "solo", p1)
			assert.Empty(t, p2)
		})

		t.Run("批量取与计数", func(t *testing.T) {
			// 清空队列，避免前面子测试残留的未到期任务干扰计数
			require.NoError(t, rdb.Del(ctx, "dq:1").Err())
			for i := 0; i < 5; i++ {
				require.NoError(t, q.Enqueue(ctx, fmt.Sprintf("b%d", i), time.Now().Add(-time.Second)))
			}

			items, err := q.DequeueBatch(ctx, 3)
			require.NoError(t, err)
			assert.Len(t, items, 3, "应一次取出 3 个到期任务")

			count, err := q.PendingCount(ctx)
			require.NoError(t, err)
			assert.Equal(t, int64(2), count, "剩余 2 个任务")

			// 剩余任务按到期顺序取出
			rest, err := q.DequeueBatch(ctx, 10)
			require.NoError(t, err)
			assert.Len(t, rest, 2)
			count, err = q.PendingCount(ctx)
			require.NoError(t, err)
			assert.Equal(t, int64(0), count)
		})

		t.Run("json payload 序列化", func(t *testing.T) {
			type task struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			}
			require.NoError(t, q.Enqueue(ctx, task{ID: 7, Name: "x"}, time.Now().Add(-time.Second)))

			payload, ok, err := q.Dequeue(ctx)
			require.NoError(t, err)
			assert.True(t, ok)
			assert.Contains(t, payload, `"id":7`)
			assert.Contains(t, payload, `"name":"x"`)
		})

		t.Run("DequeueBatch 无任务返回空", func(t *testing.T) {
			items, err := q.DequeueBatch(ctx, 5)
			require.NoError(t, err)
			assert.Empty(t, items)
		})
	})
}
