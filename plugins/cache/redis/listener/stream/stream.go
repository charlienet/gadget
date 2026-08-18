// Package stream provides a cache.Listener implementation backed by Redis
// Streams with Consumer Groups. Unlike the PubSub-based listener, this
// provides at-least-once delivery semantics: failed messages remain in the
// stream as pending entries and are re-delivered on restart.
package stream

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
	goredis "github.com/redis/go-redis/v9"
)

const (
	defaultStreamName     = "cache:invalidate"
	defaultGroupName      = "cache-group"
	defaultConsumerID     = "cache-consumer"
	readBlockTimeout      = 2 * time.Second
	chanBufSize           = 100
	maxMessagesPerRead    = 10
	maxStreamLen          = 10000            // XAdd 的 MAXLEN 上限：裁剪已 ACK 的历史条目，防止内存无限增长
	defaultPublishTimeout = 2 * time.Second  // XAdd 默认超时
	retryBaseDelay        = time.Second      // watch 重试退避起步
	retryMaxDelay         = 30 * time.Second // watch 重试退避上限
)

type Option func(*streamListener)

// WithStreamName sets the Redis Stream key used for invalidation messages.
func WithStreamName(name string) Option {
	return func(s *streamListener) {
		s.stream = name
	}
}

// WithConsumerGroup sets the consumer group name prefix. The final group name
// is always suffixed with an instance-unique identifier (hostname-pid) so that
// every instance uses its own consumer group and receives ALL messages
// (broadcast semantics), instead of messages being load-balanced across
// consumers of a shared group.
func WithConsumerGroup(name string) Option {
	return func(s *streamListener) {
		s.group = name + "-" + instanceSuffix()
	}
}

// WithConsumerID sets the consumer ID within the group.
func WithConsumerID(id string) Option {
	return func(s *streamListener) {
		s.consumer = id
	}
}

// WithPublishTimeout 设置 Publish（XAdd）的上下文超时（默认 2 秒）。
func WithPublishTimeout(d time.Duration) Option {
	return func(s *streamListener) {
		s.publishTimeout = d
	}
}

// instanceSuffix 生成实例唯一标识（hostname-pid）。缓存失效需要广播给
// 每个实例，因此每个实例必须使用独立的 consumer group；同 host 同 pid
// 的进程在同一时刻至多一个，可保证 group 名唯一。
func instanceSuffix() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}

	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

type streamListener struct {
	mu             sync.RWMutex // 保护 rdb：Initialize 可能在 watch 运行期间替换（AddPrefix 派生新 client）
	rdb            redis.Client
	stream         string
	group          string
	consumer       string
	msgChan        chan string
	close          chan struct{}
	ready          chan struct{} // 首次成功建立消费的就绪信号（Ready 语义=首次就绪）
	readyOnce      sync.Once     // 保证 ready 只 close 一次（重连/重建不重复触发）
	publishTimeout time.Duration
	once           sync.Once      // 保证 close(s.close) 只执行一次，Close 幂等
	wg             sync.WaitGroup // 等待 watch goroutine 退出
}

// NewStreamListener creates a Listener that uses Redis Streams for reliable
// cache invalidation. The consumer group is created lazily (if it doesn't
// already exist). Messages are acknowledged after being delivered to the
// Subscribe channel.
func NewStreamListener(rdb redis.Client, opts ...Option) cache.Listener {
	s := &streamListener{
		rdb:    rdb,
		stream: defaultStreamName,
		// 默认 group 名拼接实例唯一后缀：保证每实例独立 group（广播语义），
		// 避免同组消费导致失效消息被分摊到部分实例
		group:          defaultGroupName + "-" + instanceSuffix(),
		consumer:       defaultConsumerID,
		msgChan:        make(chan string, chanBufSize),
		close:          make(chan struct{}),
		ready:          make(chan struct{}),
		publishTimeout: defaultPublishTimeout,
	}
	for _, o := range opts {
		o(s)
	}

	// consumer group 不在此处创建：此时 client 尚未经过 Initialize 确定
	// 最终前缀，提前创建会导致 group 落在无前缀的 stream 上，与后续
	// XAdd/XReadGroup 使用的 key 不一致。改为在 Initialize 中创建，
	// 并以 watch 首次读取时的 NOGROUP 检测兜底。

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.watch()
	}()

	return s
}

// Initialize 收敛前缀：通过 client 前缀机制（AddPrefix 派生）统一加前缀，
// 不再手动拼接 stream key，保证 XAdd/XReadGroup/XAck/XGroupCreate 使用
// 同一个带前缀的 key。
//
// 注意：Initialize 可能在 watch goroutine 启动后才被调用（cache.New 内
// 同步执行），因此替换 rdb 需加锁；watch 每轮读取前取 rdb 快照，最迟
// 一个 readBlockTimeout（2s）内切换到带前缀的 client。
func (s *streamListener) Initialize(opt cache.Options) {
	if len(opt.Name) > 0 {
		s.mu.Lock()
		s.rdb = s.rdb.AddPrefix(opt.Name)
		s.mu.Unlock()
	}

	s.mu.RLock()
	rdb := s.rdb
	s.mu.RUnlock()

	// 用最终 client 创建 consumer group（若 stream 尚不存在则失败忽略，
	// 由 watch 首次读取时按需创建）
	_ = rdb.XGroupCreate(context.Background(), s.stream, s.group, "0").Err()
}

func (s *streamListener) Subscribe() chan string {
	return s.msgChan
}

// markReady 标记"首次成功建立消费"：close 就绪信号。
// readyOnce 保证只 close 一次——watch 的 NOGROUP 创建与后续成功读取都会
// 触发，重连/断线重建后不重新触发（Ready 语义=首次就绪）。
func (s *streamListener) markReady() {
	s.readyOnce.Do(func() { close(s.ready) })
}

// Ready 返回首次就绪信号（channel 关闭即就绪）：watch 首次成功建立消费
// （XReadGroup 成功，或 NOGROUP 后 XGroupCreateMkStream 创建成功）时触发。
//
// 与 pubsub 不同，Initialize 换 rdb 后不重置就绪信号：stream 每轮读取前
// 取 rdb 快照、失败自动退避重试（自愈），且 stream 持久化 + 消费组可补读
// 未消费消息（at-least-once），不存在 pubsub 那种"订阅建立前发布的消息
// 丢失"的时序窗口，首次就绪语义即足够。
func (s *streamListener) Ready() <-chan struct{} {
	return s.ready
}

// Publish 向 stream 追加一条失效消息。
//
// XAdd 附带 MAXLEN（maxStreamLen）：消费端 XAck 只标记消息已处理，
// 不会从 stream 中删除条目；若 XAdd 不设上限，已 ACK 的历史条目会随
// 失效消息量无限累积，导致 Redis 内存持续增长。MAXLEN 确保 stream
// 只保留最近的 maxStreamLen 条（超出部分在追加时自动裁剪）。
func (s *streamListener) Publish(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.publishTimeout)
	defer cancel()

	s.mu.RLock()
	rdb := s.rdb
	s.mu.RUnlock()

	return rdb.XAdd(ctx, &goredis.XAddArgs{
		Stream: s.stream,
		MaxLen: maxStreamLen,
		Values: map[string]interface{}{"key": key},
	}).Err()
}

// Close 优雅关闭监听器：幂等（重复调用不 panic），
// 并等待 watch goroutine 真正退出，受 ctx 超时/取消控制。
func (s *streamListener) Close(ctx context.Context) error {
	s.once.Do(func() {
		close(s.close)
	})

	// 等待 watch goroutine 退出
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *streamListener) watch() {
	// 重试退避：1s 起步、翻倍增长、上限 30s，避免故障期间的错误风暴
	retryDelay := retryBaseDelay

	for {
		select {
		case <-s.close:
			return
		default:
		}

		// 每轮读取前取 rdb 快照：Initialize 可能在运行期间替换 client
		// （AddPrefix 派生带前缀的新连接池），快照保证本轮的 XReadGroup /
		// XGroupCreateMkStream / XAck 使用同一 client，避免字段读写竞态。
		s.mu.RLock()
		rdb := s.rdb
		s.mu.RUnlock()

		result, err := rdb.XReadGroup(context.Background(), &goredis.XReadGroupArgs{
			Group:    s.group,
			Consumer: s.consumer,
			Streams:  []string{s.stream, ">"},
			Count:    int64(maxMessagesPerRead),
			Block:    readBlockTimeout,
		}).Result()

		if err != nil {
			// group 可能尚未创建（例如直接使用 listener 未经过 Initialize），
			// 检测 NOGROUP 后按需创建（MKSTREAM 确保 stream 存在）
			if goredis.HasErrorPrefix(err, "NOGROUP") {
				if cerr := rdb.XGroupCreateMkStream(context.Background(), s.stream, s.group, "0").Err(); cerr == nil {
					// group/stream 已按需建立：消费链路可用，触发就绪
					s.markReady()
					retryDelay = retryBaseDelay
					continue
				}
				// 创建失败（如 Redis 不可达）：落入下方退避重试
			}

			select {
			case <-s.close:
				return
			case <-time.After(retryDelay):
			}

			retryDelay *= 2
			if retryDelay > retryMaxDelay {
				retryDelay = retryMaxDelay
			}
			continue
		}

		// 读取成功：消费链路建立，触发就绪（首次）；重置退避
		s.markReady()
		retryDelay = retryBaseDelay

		for _, stream := range result {
			for _, msg := range stream.Messages {
				key, ok := msg.Values["key"].(string)
				if !ok || key == "" {
					continue
				}

				select {
				case s.msgChan <- key:
					// Acknowledge after delivering to channel
					rdb.XAck(context.Background(), s.stream, s.group, msg.ID)
				case <-s.close:
					return
				}
			}
		}
	}
}
