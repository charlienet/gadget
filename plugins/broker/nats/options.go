package nats

import (
	"fmt"
	"time"

	nats "github.com/nats-io/nats.go"
)

// options 是本包的内部配置，仅由 [Option] 写入、由 [New] 消费。
// 非法选项值在各 Option 返回的闭包中 panic（闭包在 New 内应用），
// 与 lifecycle 包先例一致：选项错误属于程序期错误，不应延后到运行期暴露。
type options struct {
	url      string
	name     string
	connOpts []nats.Option

	syncPublish   bool
	syncPublishTo time.Duration
	handlerErrFn  func(topic string, err error)
}

// Option 是 [New] 的配置选项。
type Option func(*options)

// WithURL 指定 NATS 服务地址（如 "nats://127.0.0.1:4222"），写入内部
// nats.Options.Url；未设置时使用 nats.DefaultURL。
//
// url 为空字符串时 panic（程序期错误）。
func WithURL(url string) Option {
	return func(o *options) {
		if url == "" {
			panic("nats: WithURL requires a non-empty url")
		}
		o.url = url
	}
}

// WithName 设置客户端名称标签（server 端 CONNECT 可见），写入内部
// nats.Options.Name；未设置时沿用 nats.GetDefaultOptions 的默认值。
//
// name 为空字符串时 panic（程序期错误）。
func WithName(name string) Option {
	return func(o *options) {
		if name == "" {
			panic("nats: WithName requires a non-empty name")
		}
		o.name = name
	}
}

// WithConnOptions 按序将原生 nats.Option 应用到内部 nats.Options，
// 是认证（nats.Token/nats.UserInfo/nats.UserCredentials）、TLS（nats.RootCAs/
// nats.ClientCert）、重连策略（nats.MaxReconnects/nats.ReconnectWait）、
// 异步错误回调（nats.ErrorHandler/nats.AsyncErrorCB）以及
// nats.DrainTimeout（Close 排空超时，默认 30s）等所有底层连接参数的唯一入口。
//
// opts 中出现 nil 元素时 panic（程序期错误）；
// 应用阶段返回的错误属于运行时失败，经 [New] 的 error 通道透出。
func WithConnOptions(opts ...nats.Option) Option {
	return func(o *options) {
		for i, fn := range opts {
			if fn == nil {
				panic(fmt.Sprintf("nats: WithConnOptions does not accept nil option (element %d)", i))
			}
		}
		o.connOpts = append(o.connOpts, opts...)
	}
}

// WithSyncPublish 开启同步发布：Publish 在本地写入成功后追加执行
// conn.FlushTimeout(timeout)，以确认服务端已接收该消息（详见 package 文档
// 的可靠性语义一节；不确认消费者已处理）。未设置时 Publish 为异步
// at-most-once 语义。
//
// timeout <= 0 时 panic（程序期错误）。
func WithSyncPublish(timeout time.Duration) Option {
	return func(o *options) {
		if timeout <= 0 {
			panic("nats: WithSyncPublish requires timeout > 0")
		}
		o.syncPublish = true
		o.syncPublishTo = timeout
	}
}

// WithHandlerErrorHandler 注册订阅 handler 错误上报的唯一出口：
// handler 返回错误或 panic（panic 被 recover 并转译 error 后）均回调
// fn(topic, err)。
//
// 未注册时 handler 的错误与 panic 转译结果一律丢弃（见 package 文档
// “订阅桥接与 handler panic”一节）。
//
// fn 为 nil 时 panic（程序期错误）。
func WithHandlerErrorHandler(fn func(topic string, err error)) Option {
	return func(o *options) {
		if fn == nil {
			panic("nats: WithHandlerErrorHandler requires a non-nil function")
		}
		o.handlerErrFn = fn
	}
}
