// Package pubsub provides a cache.Listener implementation backed by Redis
// PubSub with automatic reconnection on connection failure.
package pubsub

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/charlienet/gadget/cache"
	"github.com/charlienet/gadget/redis"
	goredis "github.com/redis/go-redis/v9"
)

const (
	chanBufSize           = 100
	healthCheckIntv       = 15 * time.Second // ping interval
	reconnectDelay        = time.Second      // delay between reconnection attempts
	defaultPublishTimeout = 2 * time.Second  // Publish 默认超时
	subscribeTimeout      = 5 * time.Second  // 订阅建立（dial+SUBSCRIBE 确认）超时
)

type Option func(*pubSubListener)

// WithPublishTimeout 设置 Publish 的上下文超时（默认 2 秒）。
func WithPublishTimeout(d time.Duration) Option {
	return func(r *pubSubListener) {
		r.publishTimeout = d
	}
}

type pubSubListener struct {
	mu             sync.RWMutex // 保护 rdb：Initialize 可能在 watch 运行期间替换（AddPrefix 派生新 client）
	rdb            redis.Client
	channel        string
	msgChan        chan string
	close          chan struct{}
	reconnect      chan struct{} // Initialize 更换 rdb 后触发 watch 重建订阅
	ready          chan struct{} // 当前 rdb 订阅就绪信号（channel 关闭=就绪）
	readyMu        sync.Mutex    // 保护 ready 的重置与 close
	readySignaled  bool          // ready 是否已 close（防重复 close panic）
	publishTimeout time.Duration
	once           sync.Once      // 保证 close(r.close) 只执行一次，Close 幂等
	wg             sync.WaitGroup // 等待 watch goroutine 退出
}

// NewListener creates a cache.Listener backed by Redis PubSub. It includes
// automatic health checks and reconnection when the connection drops.
func NewListener(rdb redis.Client, channel string, opts ...Option) cache.Listener {
	r := &pubSubListener{
		rdb:            rdb,
		channel:        channel,
		msgChan:        make(chan string, chanBufSize),
		close:          make(chan struct{}),
		reconnect:      make(chan struct{}, 1),
		ready:          make(chan struct{}),
		publishTimeout: defaultPublishTimeout,
	}
	for _, o := range opts {
		o(r)
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.watch()
	}()

	return r
}

// Initialize 为 channel 添加业务前缀（AddPrefix 派生新 client）。
//
// 注意：Initialize 可能在 watch goroutine 启动后才被调用（cache.New 内
// 同步执行），因此替换 rdb 需加锁，并通过 reconnect 信号通知 watch 用
// 新 client 重建订阅。重建完成前重置 Ready 信号：调用方在 Initialize
// 之后获取的 Ready() 会等待重建完成，避免"新前缀订阅尚未建立"时发布
// 消息丢失（pubsub at-most-once）。
func (r *pubSubListener) Initialize(opt cache.Options) {
	if len(opt.Name) == 0 {
		return
	}

	r.mu.Lock()
	r.rdb = r.rdb.AddPrefix(opt.Name)
	r.mu.Unlock()

	// 重置就绪信号：watch 用新 rdb 重建订阅成功后会重新触发
	r.resetReady()

	// 通知 watch 用新 client 重建订阅
	select {
	case r.reconnect <- struct{}{}:
	default:
	}
}

// subscribe 建立订阅并等待 SUBSCRIBE 确认（带超时）。
//
// 无超时风险：go-redis 的 Subscribe 会惰性建连，若 Redis 不可达且 dial 无
// 超时配置，连接建立可能永久阻塞。这里用带超时的 ctx 调用 Subscribe 并
// 通过 Receive 同步等待订阅确认（Receive 内部同样使用该 ctx），超时即失败。
// 失败时按现有重连节奏（reconnectDelay）重试，直到成功或监听器关闭。
func (r *pubSubListener) subscribe() (*goredis.PubSub, error) {
	for {
		r.mu.RLock()
		rdb := r.rdb
		r.mu.RUnlock()

		subCtx, cancel := context.WithTimeout(context.Background(), subscribeTimeout)
		sub := rdb.Subscribe(subCtx, r.channel)
		_, err := sub.Receive(subCtx)
		cancel()
		if err != nil {
			_ = sub.Close()

			select {
			case <-r.close:
				return nil, errors.New("pubsub: listener closed while subscribing")
			case <-time.After(reconnectDelay):
			}
			continue
		}

		// 订阅建立期间 rdb 可能已被 Initialize 替换（AddPrefix 派生新 client）：
		// 此时返回的 sub 基于旧 rdb（channel 无前缀），与后续 Publish 的
		// 前缀不一致会导致消息丢失，故丢弃本次订阅并用新 rdb 重试。
		r.mu.RLock()
		cur := r.rdb
		r.mu.RUnlock()
		if cur != rdb {
			_ = sub.Close()
			continue
		}

		// 当前 rdb 的订阅就绪：触发 Ready 信号（幂等；Initialize 重置后
		// 会重新触发，表示重建后的订阅也已就绪）。
		r.markReady()
		return sub, nil
	}
}

// markReady 标记"当前 rdb 的订阅已就绪"：close 当前 ready channel。
// 幂等（已 close 过则跳过）；Initialize 换 rdb 后 resetReady 会新建
// channel，重建订阅成功时再次触发。
func (r *pubSubListener) markReady() {
	r.readyMu.Lock()
	defer r.readyMu.Unlock()
	if !r.readySignaled {
		close(r.ready)
		r.readySignaled = true
	}
}

// resetReady 重置就绪信号：Initialize 更换 rdb 后调用，
// 使调用方在 Initialize 之后获取的 Ready() 等待重建订阅完成。
func (r *pubSubListener) resetReady() {
	r.readyMu.Lock()
	defer r.readyMu.Unlock()
	r.ready = make(chan struct{})
	r.readySignaled = false
}

func (r *pubSubListener) watch() {
	sub, err := r.subscribe()
	if err != nil {
		return // 监听器已关闭
	}
	defer sub.Close()

	// 记录当前 sub 建立时的 rdb：用于识别 Initialize 换 rdb 后是否需要重建。
	// subscribe() 成功时已复查 rdb 一致性，故此刻 r.rdb 与 sub 基于同一 client。
	r.mu.RLock()
	subRdb := r.rdb
	r.mu.RUnlock()

	c := sub.Channel()
	healthTicker := time.NewTicker(healthCheckIntv)
	defer healthTicker.Stop()

	for {
		select {
		case <-r.reconnect:
			// Initialize 已更换 client（AddPrefix）。仅当订阅确实基于旧 rdb
			// 时才重建；若当前 sub 已基于最新 rdb（例如 watch 首次 subscribe
			// 时 Initialize 已完成，subscribe 内部复查后已用新 rdb），忽略
			// 冗余信号——不必要的重建会关闭已就绪的订阅，导致其缓冲中的
			// 消息（发布在就绪之后、重建之前）被丢弃。
			r.mu.RLock()
			cur := r.rdb
			r.mu.RUnlock()
			if cur == subRdb {
				continue
			}

			// 用新 rdb 重建订阅：先建立新订阅再关闭旧的，缩短无订阅窗口；
			// subscribe() 内部会取当前 rdb 快照并复查一致性，重建成功即
			// 触发 Ready。
			newSub, err := r.subscribe()
			if err != nil {
				return
			}
			_ = sub.Close()
			sub = newSub
			subRdb = cur
			c = sub.Channel()
			continue
		case msg, ok := <-c:
			if !ok {
				// channel 被关闭（订阅连接异常断开）：重建订阅并重连
				_ = sub.Close()
				if sub, err = r.subscribe(); err != nil {
					return
				}
				r.mu.RLock()
				subRdb = r.rdb
				r.mu.RUnlock()
				c = sub.Channel()
				continue
			}
			if msg != nil {
				// 关闭期间不阻塞发送，避免 watch 无法退出
				select {
				case r.msgChan <- msg.Payload:
				case <-r.close:
					return
				}
			}
		case <-healthTicker.C:
			// Periodic health check: if ping fails, reconnect.
			// Ping 同样使用带超时的 ctx，避免 dial 无超时时永久阻塞 watch。
			pingCtx, cancel := context.WithTimeout(context.Background(), subscribeTimeout)
			err := sub.Ping(pingCtx)
			cancel()
			if err != nil {
				_ = sub.Close()
				if sub, err = r.subscribe(); err != nil {
					return
				}
				r.mu.RLock()
				subRdb = r.rdb
				r.mu.RUnlock()
				c = sub.Channel()
			}
		case <-r.close:
			_ = sub.Close()
			return
		}
	}
}

func (r *pubSubListener) Subscribe() chan string {
	return r.msgChan
}

// Ready 返回订阅就绪信号（channel 关闭即就绪）。
//
// NewListener 异步启动订阅 goroutine，返回时订阅可能尚未建立；PubSub 广播
// 为 at-most-once（订阅建立前发布的消息无法补收）。新启动的实例在对外提供
// 服务前可等待该信号（带调用方超时），避免失效消息在订阅建立前发布而丢失。
//
// 信号语义为"当前 rdb 的订阅就绪"：若 Initialize 更换了 client（AddPrefix），
// 重建订阅完成前 Ready 不会就绪——在 cache.New 之后获取 Ready() 并等待，
// 可确保 Initialize 之后发布的消息不丢失。
func (r *pubSubListener) Ready() <-chan struct{} {
	r.readyMu.Lock()
	defer r.readyMu.Unlock()
	if r.ready == nil {
		r.ready = make(chan struct{})
	}
	return r.ready
}

func (r *pubSubListener) Publish(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), r.publishTimeout)
	defer cancel()

	r.mu.RLock()
	rdb := r.rdb
	r.mu.RUnlock()

	return rdb.Publish(ctx, r.channel, key).Err()
}

// Close 优雅关闭监听器：幂等（重复调用不 panic），
// 并等待 watch goroutine 真正退出，受 ctx 超时/取消控制。
func (r *pubSubListener) Close(ctx context.Context) error {
	r.once.Do(func() {
		close(r.close)
	})

	// 等待 watch goroutine 退出
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
