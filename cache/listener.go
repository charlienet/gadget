package cache

import "context"

// Listener 定义缓存失效通知的订阅/发布接口。
// Publish 必须在 Close 前完成；实现须保证 Publish 与 Close 并发安全。
type Listener interface {
	Subscribe() chan string
	Publish(key string) error
	// Ready 返回监听器首次就绪信号（订阅/连接建立完成）。新实例对外服务前
	// 可等待该信号，避免就绪前发布的失效消息丢失。实现必须幂等/线程安全。
	Ready() <-chan struct{}
	// Close 优雅关闭监听器，等待内部 goroutine 退出，受 ctx 超时/取消控制。
	// 实现必须幂等：重复调用不 panic。
	Close(ctx context.Context) error
}
