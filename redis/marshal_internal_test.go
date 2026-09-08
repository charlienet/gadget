package redis

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// testBinaryMarshaler 实现 encoding.BinaryMarshaler，覆盖金表的接口分支：
// marshalItem 必须原样透传 MarshalBinary 的结果与错误。
type testBinaryMarshaler struct {
	payload []byte
	err     error
}

func (t testBinaryMarshaler) MarshalBinary() ([]byte, error) {
	if t.err != nil {
		return nil, t.err
	}
	return t.payload, nil
}

// TestMarshalItemGolden 是 marshalItem 的格式金表：逐类型钉死编码字节。
//
// 这些期望值按 go-redis v9.22 internal/proto/writer.go 的 WriteArg 行为
// 手工推导（int/uint 十进制文本、float AppendFloat('f',-1,64)、bool→"1"/
// "0"、time.Time→RFC3339Nano、time.Duration→纳秒整数、net.IP 原始字节、
// BinaryMarshaler 透传、nil→空）。金表是存量过滤器数据的格式契约（见
// marshal.go 冻结声明）：任何改动使本测试变红即意味着 breaking——除非
// 同时提供过滤器重建迁移方案并升主版本。
//
// 与真实 go-redis 序列化链路的逐字节对照见 marshal_writer_parity_test.go
// （漂移防线）；本表锁死"我们承诺的格式"，parity 表锁死"go-redis 的实际
// 格式未漂移"，两者互为印证。
func TestMarshalItemGolden(t *testing.T) {
	// 固定时刻（避免 time.Now 的精度尾数差异）；RFC3339Nano 省略零小数位。
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	tsNano := time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC)

	cases := []struct {
		name string
		in   any
		want []byte
	}{
		{"nil", nil, []byte{}},
		{"string/hello", "hello", []byte("hello")},
		{"string/empty", "", []byte{}},
		{"bytes/raw", []byte{0x00, 0xde, 0xad, 0x00}, []byte{0x00, 0xde, 0xad, 0x00}},
		{"bytes/empty", []byte{}, []byte{}},
		{"int/neg", int(-42), []byte("-42")},
		{"int/zero", int(0), []byte("0")},
		{"int8/min", int8(-128), []byte("-128")},
		{"int16/max", int16(32767), []byte("32767")},
		{"int32/min", int32(-2147483648), []byte("-2147483648")},
		{"int64/max", int64(9223372036854775807), []byte("9223372036854775807")},
		{"uint/42", uint(42), []byte("42")},
		{"uint8/max", uint8(255), []byte("255")},
		{"uint16/max", uint16(65535), []byte("65535")},
		{"uint32/max", uint32(4294967295), []byte("4294967295")},
		{"uint64/max", uint64(18446744073709551615), []byte("18446744073709551615")},
		// float32 先转 float64 再按 bitSize=64 格式化（writer 同构）：
		// 1.5 可精确表示，避免 1.1 这类 float32→float64 展开噪声混入金表。
		{"float32/1.5", float32(1.5), []byte("1.5")},
		// 整数值的 float64 输出不带小数点（'f' + prec=-1 的最短表示）。
		{"float64/1.0", float64(1.0), []byte("1")},
		{"float64/-0.5", float64(-0.5), []byte("-0.5")},
		// bool 是数字编码，不是 "true"/"false" 文本。
		{"bool/true", true, []byte("1")},
		{"bool/false", false, []byte("0")},
		{"time/RFC3339Nano", ts, []byte("2024-01-02T03:04:05Z")},
		{"time/nanos", tsNano, []byte("2024-01-02T03:04:05.123456789Z")},
		{"duration/1s", time.Second, []byte("1000000000")},
		{"duration/neg", -3 * time.Second, []byte("-3000000000")},
		// net.IP 原样透传底层字节（4 或 16 字节），绝不做 "127.0.0.1" 文本化。
		// 下面三条分别钉住：4 字节 raw、net.IPv4(...).To4() 归一的 4 字节、
		// 以及 net.IPv4(...) 不归一的 16 字节 v4-in-v6 形态。
		{"net.IP/v4-raw", net.IP{127, 0, 0, 1}, []byte{127, 0, 0, 1}},
		{"net.IP/v4-constructed", net.IPv4(127, 0, 0, 1).To4(), []byte{127, 0, 0, 1}},
		// net.IPv4() 直接构造（不做 .To4()）是 16 字节 v4-in-v6 形态；
		// marshalItem 与 go-redis writer 都原样透传底层字节——两边都**不
		// 归一化**（不 To4、不文本化），线上字节必须一致。
		{"net.IP/v4-in-v6-unnormalized", net.IPv4(127, 0, 0, 1), []byte{
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 127, 0, 0, 1,
		}},
		{"net.IP/v6-loopback", net.IPv6loopback, []byte{
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1,
		}},
		{"binaryMarshaler", testBinaryMarshaler{payload: []byte{0xde, 0xad}}, []byte{0xde, 0xad}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := marshalItem(c.in)
			if err != nil {
				t.Fatalf("marshalItem(%#v) 意外报错：%v", c.in, err)
			}
			if !bytes.Equal(got, c.want) {
				t.Fatalf("编码不符：%q got % x want % x（格式漂移——存量数据失效，属 breaking）", c.name, got, c.want)
			}
		})
	}

	// 空 IP 的 nil 与零长：net.IP(nil) 走 net.IP case 得零长（非报错、非 panic）。
	got, err := marshalItem(net.IP(nil))
	if err != nil {
		t.Fatalf("net.IP(nil) 不应报错：%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("net.IP(nil) 应编码为空字节，got % x", got)
	}
}

// unsupportedStruct 是具名 struct 类型，验证走 default 报错（匿名 struct
// 与具名 struct 均不支持）。
type unsupportedStruct struct {
	X int
}

// TestMarshalItemUnsupported 断言不支持类型一律返回数据类错误
// （含 "can't marshal"，不 panic），且 MarshalBinary 的错误原样透传。
// 指针变体（*string 等）刻意不支持（writer 有 case，本函数走 default，
// 差异理由见 marshal.go 注释）。
func TestMarshalItemUnsupported(t *testing.T) {
	str := "deref-me"
	ch := make(chan int)

	cases := []struct {
		name string
		in   any
	}{
		{"anon-struct", struct{ X int }{1}},
		{"named-struct", unsupportedStruct{X: 2}},
		{"slice-of-int", []int{1, 2, 3}},
		{"map", map[string]string{"k": "v"}},
		{"complex64", complex64(1 + 2i)},
		{"chan", ch},
		{"ptr-string", &str},
		{"ptr-string-nil", (*string)(nil)},
		{"ptr-int", new(int)},
		{"func", func() {}},
		// 切片元素类型可编码不代表切片可编码（[]byte 有专属 case，
		// []string 走 default）。
		{"slice-of-string", []string{"a", "b"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := marshalItem(c.in)
			if err == nil {
				t.Fatalf("marshalItem(%T) 应报错，got % x", c.in, got)
			}
			if !strings.Contains(err.Error(), "can't marshal") {
				t.Fatalf("错误文案应含 can't marshal，got %v", err)
			}
			if got != nil {
				t.Fatalf("报错时不应返回字节，got % x", got)
			}
			// 规格 11：数据类错误不得被 IsUnavailable 判为服务不可用
			// （否则会误触 FailPolicy 兜底掩盖调用方参数错误）。
			if IsUnavailable(err) {
				t.Fatalf("marshal 错误不得判为 Unavailable：%v", err)
			}
		})
	}
}

// TestMarshalItemBinaryMarshalerError 验证 MarshalBinary 的错误原样透传
// （不被包装成 can't marshal——调用方需看到模块自身的失败原因）。
func TestMarshalItemBinaryMarshalerError(t *testing.T) {
	sentinel := errors.New("boom")
	_, err := marshalItem(testBinaryMarshaler{err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("MarshalBinary 错误应原样透传，got %v", err)
	}
}

// TestMarshalItemStringBytesParity 断言 string 与等值 []byte 编码字节相同，
// 进而位哈希（bitmap hashs）结果相同——item any 化后，调用方以 string 写、
// 以 []byte 读（或反之）必须命中同一批位，否则过滤器语义被破坏。
func TestMarshalItemStringBytesParity(t *testing.T) {
	b := newSimBitmap(t, 100_000, 0.01)

	samples := []string{
		"",
		"hello",
		"bloom:item:42",
		"\x00\x01\xff binary-ish",
		"utf8-中文-😀",
	}

	for _, s := range samples {
		t.Run(fmt.Sprintf("%q", s), func(t *testing.T) {
			fromString, err := marshalItem(s)
			if err != nil {
				t.Fatalf("string 编码：%v", err)
			}
			fromBytes, err := marshalItem([]byte(s))
			if err != nil {
				t.Fatalf("[]byte 编码：%v", err)
			}
			if !bytes.Equal(fromString, fromBytes) {
				t.Fatalf("string/%s 与 []byte 编码分叉：% x vs % x", s, fromString, fromBytes)
			}

			posStr, err := b.hashs(s)
			if err != nil {
				t.Fatalf("hashs(string)：%v", err)
			}
			posBytes, err := b.hashs([]byte(s))
			if err != nil {
				t.Fatalf("hashs([]byte)：%v", err)
			}
			if len(posStr) != len(posBytes) {
				t.Fatalf("位置数分叉：%d vs %d", len(posStr), len(posBytes))
			}
			for i := range posStr {
				if posStr[i] != posBytes[i] {
					t.Fatalf("第 %d 位分叉：%d vs %d（string/[]byte 哈希口径必须一致）", i, posStr[i], posBytes[i])
				}
			}
		})
	}
}
