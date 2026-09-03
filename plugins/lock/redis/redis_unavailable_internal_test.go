package redis

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/charlienet/gadget/lock"
	r "github.com/charlienet/gadget/redis"
)

// 本文件为离线（不依赖真实 Redis）的同包（package redis）测试，覆盖 Backend
// 委托核心分类函数 r.IsUnavailable 的错误分诊契约。用例表与
// plugins/ratelimit/redis/redis_internal_test.go 的 TestIsUnavailable 同源。

// timeoutNetErr 实现 net.Error 且 Timeout()==true，用于覆盖读/写超时分类。
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return false }

// TestIsUnavailableClassification 表驱动断言 r.IsUnavailable 对各错误形态的分类，
// 与 Backend 各方法内部实际使用的判定完全一致。
func TestIsUnavailableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"连接拒绝 OpError", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"io.EOF", io.EOF, true},
		{"意外 EOF", io.ErrUnexpectedEOF, true},
		{"net 超时", timeoutNetErr{}, true},
		{"连接池超时", errors.New("redis: connection pool timeout"), true},
		{"client 已关闭", errors.New("redis: client is closed"), true},
		{"调用方取消", context.Canceled, false},
		{"调用方超时", context.DeadlineExceeded, false},
		{"命令级错误", errors.New("WRONGTYPE Operation against a key holding the wrong kind of value"), false},
		{"Lua 运行错误", errors.New("ERR Error in script"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.IsUnavailable(tc.err); got != tc.want {
				t.Fatalf("r.IsUnavailable(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// newUnreachableClient 指向必然被拒的端口（连接层故障，无需真实 Redis）。
// MaxRetries=-1 关闭 go-redis 重试，拨号失败即刻返回，保证测试快速稳定。
func newUnreachableClient() *goredis.Client {
	return goredis.NewClient(&goredis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 300 * time.Millisecond,
		MaxRetries:  -1,
	})
}

// TestTryAcquireUnavailableWrapping 断言 Redis 服务不可用时 TryAcquire 将
// 连接层故障包装为 lock.ErrBackendUnavailable，交由核心按 FailPolicy 兜底。
func TestTryAcquireUnavailableWrapping(t *testing.T) {
	client := newUnreachableClient()
	defer client.Close()
	b := New(client)

	ok, err := b.TryAcquire(context.Background(), "gadget-lock-test:unavailable", "tok", time.Second)
	if err == nil {
		t.Fatal("不可达后端必须报错")
	}
	if ok {
		t.Fatalf("不可达时不得报告获取成功，got ok=%v", ok)
	}
	if !errors.Is(err, lock.ErrBackendUnavailable) {
		t.Fatalf("连接类故障应包装 ErrBackendUnavailable，got %v", err)
	}
}

// TestTryAcquireCtxCancelNotWrapped 断言 ctx 取消路径不兜底：返回的错误不得含
// lock.ErrBackendUnavailable（Backend 错误契约第三条，对齐 ratelimit 侧同名约定）。
func TestTryAcquireCtxCancelNotWrapped(t *testing.T) {
	client := newUnreachableClient()
	defer client.Close()
	b := New(client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.TryAcquire(ctx, "gadget-lock-test:ctxcancel", "tok", time.Second)
	if err == nil {
		t.Fatal("ctx 取消时 SetNX 应返回错误")
	}
	if errors.Is(err, lock.ErrBackendUnavailable) {
		t.Fatalf("ctx 取消不得包装为 ErrBackendUnavailable（契约三条之三条），got %v", err)
	}
}
