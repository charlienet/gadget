// Package nats provides a [github.com/charlienet/gadget/broker] implementation
// backed by NATS Core (pub/sub).
//
// # 可靠性语义
//
// 默认（未开启 [WithSyncPublish]）时 Publish 为异步 at-most-once：返回 nil
// 仅表示消息已写入客户端本地发送缓冲，不代表服务端已收到；连接断开时缓冲中
// 的消息可能永久丢失（NATS Core 不重发未确认的发布消息）。异步路径的错误
// （如服务端对发布的负面确认）本包不设默认回调，需要感知时通过
// [WithConnOptions](nats.ErrorHandler(...)) 或 nats.AsyncErrorCB 自行配置。
//
// 开启 [WithSyncPublish](timeout) 后，Publish 在本地写入成功后追加执行
// conn.FlushTimeout(timeout)，成功返回表示确认服务端已接收该消息（与
// server 一次往返）。注意：这不确认任何消费者已处理该消息，不构成
// at-least-once 语义——NATS Core 下消费者离线时消息即丢失。
//
// # 订阅桥接与 handler panic
//
// Subscribe 的回调对每条到达消息真实调用 handler，且每条消息构造独立的
// broker.Event 与 broker.Message 实例。handler 运行在 nats.go 库内部
// goroutine 中，因此本包对 handler 的 panic 采取 recover 并转译为 error
// （re-panic 的唯一后果是整个进程崩溃），与 handler 自身返回的错误一样，
// 统一经 [WithHandlerErrorHandler] 注册的出口上报。
//
// 未注册 WithHandlerErrorHandler 时，handler 返回的错误与 panic 转译结果
// 一律静默丢弃。
//
// # Ack 语义
//
// NATS Core 无应用层 ACK（at-most-once），[broker.Event] 的 Ack 方法为接口
// 占位、恒返回 nil；真正的 ACK 语义等待 broker v0.2.0 与 JetStream 支持时
// 引入。Event.Error 恒返回 nil：handler 错误不经 Event 回流，
// [WithHandlerErrorHandler] 是唯一出口。
//
// # 破坏性变更（v0.2.0）
//
// 旧签名 New() broker.Broker 已移除，改为 New(opts ...Option)
// (broker.Broker, error)：
//   - 支持通过 Option 配置连接地址、名称与全部原生 nats.Option；
//   - 连接失败返回 error，不再静默返回不可用实例（v0.1.x 会吞掉连接错误）。
//
// New 的 error 通道只承载运行时失败（连接失败、底层 Option 应用失败等）；
// 程序期错误（非法 Option 值）直接 panic，对齐 lifecycle 包先例。
package nats

import (
	"fmt"
	"sync"

	"github.com/charlienet/gadget/broker"
	nats "github.com/nats-io/nats.go"
)

var (
	_ broker.Broker     = (*natsBroker)(nil)
	_ broker.Event      = (*event)(nil)
	_ broker.Subscriber = (*subscriber)(nil)
)

type natsBroker struct {
	conn *nats.Conn
	opts options

	closeOnce sync.Once // 保证 Drain 只执行一次（Close 幂等）
}

// subscriber 实现 broker.Subscriber，包装底层 NATS subscription。
type subscriber struct {
	s *nats.Subscription
}

// event 实现 broker.Event。字段布局对齐 plugins/broker/redis 的 event 先例。
type event struct {
	topic   string
	message *broker.Message
	err     error
}

// New 建立到 NATS server 的连接并返回 broker。
//
// 返回的 error 只承载运行时失败（连接失败、WithConnOptions 应用失败）；
// 非法 Option 值（见各 Option 文档）在应用阶段直接 panic，属程序期错误。
func New(opts ...Option) (broker.Broker, error) {
	var o options
	for _, fn := range opts {
		fn(&o)
	}

	nopts := nats.GetDefaultOptions()
	if o.url == "" {
		o.url = nats.DefaultURL
	}
	nopts.Url = o.url
	if o.name != "" {
		nopts.Name = o.name
	}
	for _, fn := range o.connOpts {
		if err := fn(&nopts); err != nil {
			return nil, fmt.Errorf("nats: apply conn option failed: %w", err)
		}
	}

	conn, err := nopts.Connect()
	if err != nil {
		return nil, fmt.Errorf("nats: connect to %q failed: %w", nopts.Url, err)
	}

	return &natsBroker{conn: conn, opts: o}, nil
}

// toMessage 是 nats.Msg 到 broker.Message 的唯一映射函数。
// broker v0.2.0 为 Message 增加 Header 时只需修改此处。
func toMessage(msg *nats.Msg) *broker.Message {
	return &broker.Message{Body: string(msg.Data)}
}

// handlerError 是 handler 返回错误 / panic 转译结果的唯一出口：
// 未注册 WithHandlerErrorHandler 时丢弃。
func (b *natsBroker) handlerError(topic string, err error) {
	if b.opts.handlerErrFn != nil {
		b.opts.handlerErrFn(topic, err)
	}
}

// Publish 向 topic 发布消息。
//
// 默认异步 at-most-once：返回 nil 仅表示消息进入本地发送缓冲，
// error 原样透传底层 nats 错误（含 Close 后的 nats.ErrConnectionClosed）。
// 开启 WithSyncPublish 后追加 conn.FlushTimeout，确认服务端已接收
// （不确认消费者已处理），见 package 文档。
func (b *natsBroker) Publish(topic string, msg *broker.Message) error {
	// 防御分支：本包 New 不产生 conn==nil 的实例；语义与已关闭连接一致，
	// 透传 nats 库错误而非自造错误。
	if b.conn == nil {
		return nats.ErrConnectionClosed
	}

	if err := b.conn.Publish(topic, []byte(msg.Body)); err != nil {
		return err
	}
	if b.opts.syncPublish {
		return b.conn.FlushTimeout(b.opts.syncPublishTo)
	}

	return nil
}

// Subscribe 订阅 topic（支持 NATS 通配符 subject），每条消息到达时调用
// handler：构造独立的 event + Message，handler 返回错误或 panic 时经
// WithHandlerErrorHandler 上报（未注册则丢弃），见 package 文档。
func (b *natsBroker) Subscribe(topic string, handler broker.Handler) (broker.Subscriber, error) {
	if b.conn == nil {
		return nil, nats.ErrConnectionClosed
	}

	// 桥接回调：handler 必须被真实调用。nats msgHandler 运行在 nats.go 库
	// 内部 goroutine，panic 若 re-panic 将击穿整个进程，故 recover 转 error。
	fn := func(msg *nats.Msg) {
		defer func() {
			if r := recover(); r != nil {
				b.handlerError(msg.Subject, fmt.Errorf("nats: handler panicked: %v", r))
			}
		}()
		if err := handler(&event{topic: msg.Subject, message: toMessage(msg)}); err != nil {
			b.handlerError(msg.Subject, err)
		}
	}

	sub, err := b.conn.Subscribe(topic, fn)
	if err != nil {
		return nil, err
	}

	return &subscriber{s: sub}, nil
}

// Name 返回 broker 实现名称。
func (b *natsBroker) Name() string { return "nats" }

// Close 通过 conn.Drain 优雅关闭：排空所有订阅的在途消息与发布缓冲后再关闭
// 连接（排空超时默认 30s，调整走 WithConnOptions(nats.DrainTimeout(d))）。
//
// 底层库的 Drain 是异步的：Close 返回后连接仍在向 CLOSED 过渡，过渡期结束后
// 一切操作返回 nats.ErrConnectionClosed；需要精确感知关闭完成用
// WithConnOptions(nats.ClosedHandler(...))。
//
// Close 幂等（sync.Once），第二次及以后的调用返回 nil。
func (b *natsBroker) Close() error {
	var err error
	b.closeOnce.Do(func() {
		if b.conn != nil {
			err = b.conn.Drain()
		}
	})
	return err
}

// Topic 返回订阅的 subject。
func (s *subscriber) Topic() string { return s.s.Subject }

// Unsubscribe 立即停止投递底层订阅（in-flight 消息尽力而为，
// 需要严格排空语义请在 Broker 级别使用 Close/Drain）。
func (s *subscriber) Unsubscribe() error { return s.s.Unsubscribe() }

// Topic 返回消息实际投递到的 subject（通配符订阅时为真实匹配到的 subject，
// 而非订阅参数）。
func (e *event) Topic() string { return e.topic }

// Message 返回本事件独立构造的消息实例。
func (e *event) Message() *broker.Message { return e.message }

// Ack 恒返回 nil：NATS core 无应用层 ACK（at-most-once），本方法为接口占位；
// ACK 语义等 broker v0.2.0 与 JetStream。
func (e *event) Ack() error { return nil }

// Error 恒返回 nil：handler 错误不经 Event 回流，
// 唯一出口是 WithHandlerErrorHandler。err 字段仅为对齐 redis event 先例保留。
func (e *event) Error() error { return nil }
