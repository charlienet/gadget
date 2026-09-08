package redis

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
	goredis "github.com/redis/go-redis/v9"
)

// TestMarshalWriterParity 是 marshalItem 与 go-redis 真实序列化链路的
// 逐字节对照测试——格式契约的**漂移防线**。
//
// 原理：金表（marshal_internal_test.go）钉死的是"我们承诺的字节"，其
// 期望值按 go-redis v9.22 internal/proto/writer.go 的 WriteArg 手工推导；
// 若 go-redis 升级改了 WriteArg（如 bool 改文本、float 换格式），金表
// 不会自己变红，只有对照真实链路才会。本测试用 goredis.NewClient 连
// miniredis，把每个金表值经 client.Set 走完整 writer 编码发到服务端，
// 再从 miniredis 存储层取回原始字符串，断言与 marshalItem 字节一致——
// go-redis 格式漂移时本测试必红，从而在升级依赖时立刻暴露"存量过滤器
// 数据即将失效"这一 breaking 风险（应对见 marshal.go 冻结声明）。
//
// 同一 item 在 BF.*/CF.* 路径由 go-redis writer 编码、在 bitmap/回退
// cuckoo 路径由 marshalItem 编码后哈希，两者必须同口径，否则分片路由与
// 位哈希会随实现路径分叉。
func TestMarshalWriterParity(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("启动 miniredis：%v", err)
	}
	defer mr.Close()

	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	const key = "marshal:parity"

	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	tsNano := time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC)

	values := []struct {
		name string
		v    any
	}{
		{"nil", nil},
		{"string/hello", "hello"},
		{"string/empty", ""},
		{"bytes/raw", []byte{0x00, 0xde, 0xad, 0x00}},
		{"bytes/empty", []byte{}},
		{"int/neg", int(-42)},
		{"int/zero", int(0)},
		{"int8/min", int8(-128)},
		{"int16/max", int16(32767)},
		{"int32/min", int32(-2147483648)},
		{"int64/max", int64(9223372036854775807)},
		{"uint/42", uint(42)},
		{"uint8/max", uint8(255)},
		{"uint16/max", uint16(65535)},
		{"uint32/max", uint32(4294967295)},
		{"uint64/max", uint64(18446744073709551615)},
		{"float32/1.5", float32(1.5)},
		{"float64/1.0", float64(1.0)},
		{"float64/-0.5", float64(-0.5)},
		{"bool/true", true},
		{"bool/false", false},
		{"time/RFC3339Nano", ts},
		{"time/nanos", tsNano},
		{"duration/1s", time.Second},
		{"duration/neg", -3 * time.Second},
		{"net.IP/v4-raw", net.IP{127, 0, 0, 1}},
		{"net.IP/v4-constructed", net.IPv4(127, 0, 0, 1).To4()},
		// 不归一形态：net.IPv4() 直接传入（16 字节 v4-in-v6）。writer 与
		// marshalItem 都必须原样透传底层字节，两边都不做 To4/文本化——
		// 本用例断言线上字节一致，任何一侧偷偷归一都会变红。
		{"net.IP/v4-in-v6-unnormalized", net.IPv4(127, 0, 0, 1)},
		// net.IPv6loopback（::1）的 16 字节里前 15 字节是 \x00；miniredis 的
		// 存储与 RESP 传输均为二进制安全（长度前缀），实测可原样往返，
		// 故无需跳过——若未来该值因 miniredis 存取限制失败，在此跳过
		// 并在注释说明，其余用例必须全过。
		{"net.IP/v6-loopback", net.IPv6loopback},
		{"binaryMarshaler", testBinaryMarshaler{payload: []byte{0xde, 0xad}}},
	}

	for _, c := range values {
		t.Run(c.name, func(t *testing.T) {
			data, err := marshalItem(c.v)
			if err != nil {
				t.Fatalf("marshalItem(%T)：%v", c.v, err)
			}

			// 先删键：nil/"" 编码为空字节，避免与"键不存在"混淆。
			if err := client.Del(ctx, key).Err(); err != nil {
				t.Fatalf("清理键：%v", err)
			}
			if err := client.Set(ctx, key, c.v, 0).Err(); err != nil {
				t.Fatalf("go-redis Set(%T)：%v", c.v, err)
			}

			got, err := mr.Get(key)
			if err != nil {
				t.Fatalf("miniredis Get(%s)：%v（若为二进制字节存取限制，按注释跳过该值）", key, err)
			}
			if got != string(data) {
				t.Fatalf("go-redis writer 与 marshalItem 字节分叉：%q writer=% x marshal=% x"+
					"（go-redis 序列化格式漂移，存量过滤器数据面临失效——见 marshal.go 冻结声明）",
					c.name, got, data)
			}
		})
	}
}
