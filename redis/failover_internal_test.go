package redis

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
)

// TestIsUnavailable 验证服务不可用错误判定：
// 网络/连接池/超时类错误判定为不可用（触发兜底）；
// 命令级错误与 ctx 取消/超时不判定（不触发兜底）。
func TestIsUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "ctx 取消不判定", err: context.Canceled, want: false},
		{name: "ctx 超时不判定", err: context.DeadlineExceeded, want: false},
		{
			name: "dial 失败判定",
			err:  &net.OpError{Op: "dial", Err: errors.New("connection refused")},
			want: true,
		},
		{
			name: "连接重置判定",
			err:  &net.OpError{Op: "read", Err: errors.New("connection reset by peer")},
			want: true,
		},
		{name: "EOF 判定", err: io.EOF, want: true},
		{name: "连接池超时判定", err: errors.New("redis: connection pool timeout"), want: true},
		{name: "连接已关闭判定", err: errors.New("redis: client is closed"), want: true},
		{
			name: "i/o timeout 判定",
			err:  &net.OpError{Op: "read", Err: errors.New("i/o timeout")},
			want: true,
		},
		{
			name: "命令级错误不判定（WRONGTYPE）",
			err:  errors.New("WRONGTYPE Operation against a key holding the wrong kind of value"),
			want: false,
		},
		{
			name: "命令级错误不判定（语法）",
			err:  errors.New("ERR unknown command 'FOO'"),
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUnavailable(c.err); got != c.want {
				t.Errorf("isUnavailable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
