package redis

import (
	"encoding"
	"fmt"
	"net"
	"strconv"
	"time"
)

// marshalItem 把 BloomFilter / CuckooFilter 的 item（any）编码为规范字节
// （canonical bytes），用于位哈希（xxh3-128）与集群分片路由（xxh3-128
// .Hi % n）与 cuckoo 指纹/桶索引（xxh3-64）。
//
// 格式契约（对齐并冻结）：本函数的编码规则与 go-redis v9.22
// internal/proto/writer.go 的 Writer.WriteArg 逐类型对齐——同一 item 经
// marshalItem 得到的字节，与 go-redis 把同一值作为命令参数发往服务端的
// 字节完全一致（回归防线见 marshal_writer_parity_test.go：真实 go-redis
// 序列化链路与本函数逐字节对照，go-redis 升级导致格式漂移时该测试必红）。
//
// ⚠️ 该格式是**存量数据格式**：已写入过滤器的位图/分片归属由这些字节
// 决定。任何改变编码结果的修改（新增类型、调整 float 格式、改 bool 为
// "true"/"false" 文本等）都会使存量过滤器读到错位的位、item 路由到别的
// 分片，等同于全量失效——属 **breaking change**，须走 major（或 v0.x 的
// minor）版本发布，并与"重建过滤器"迁移方案一并说明。
//
// 与 go-redis writer 的有意差异：不支持指针变体（*string、*int 等在
// writer 中有 case，此处走 default 报错）——过滤器 item 传指针几乎必为
// 误用，静默解引用反而掩盖错误。
//
// 不支持的类型返回 fmt.Errorf 数据类错误（"redis: can't marshal %T ..."），
// 不含 IsUnavailable 识别的连接池/网络关键词，不会被误判为服务不可用而
// 触发 FailPolicy 兜底——参数编码失败是调用方错误，必须原样返回。
func marshalItem(v any) ([]byte, error) {
	switch v := v.(type) {
	case nil:
		return []byte{}, nil
	case string:
		return []byte(v), nil
	case []byte:
		return v, nil
	case int:
		return strconv.AppendInt(nil, int64(v), 10), nil
	case int8:
		return strconv.AppendInt(nil, int64(v), 10), nil
	case int16:
		return strconv.AppendInt(nil, int64(v), 10), nil
	case int32:
		return strconv.AppendInt(nil, int64(v), 10), nil
	case int64:
		return strconv.AppendInt(nil, v, 10), nil
	case uint:
		return strconv.AppendUint(nil, uint64(v), 10), nil
	case uint8:
		return strconv.AppendUint(nil, uint64(v), 10), nil
	case uint16:
		return strconv.AppendUint(nil, uint64(v), 10), nil
	case uint32:
		return strconv.AppendUint(nil, uint64(v), 10), nil
	case uint64:
		return strconv.AppendUint(nil, v, 10), nil
	case float32:
		// bitSize 恒 64（先转 float64 再格式化），与 go-redis writer.float
		// 一致：float32(1.1) 会呈现 float64 展开值 "1.100000023841858"。
		return strconv.AppendFloat(nil, float64(v), 'f', -1, 64), nil
	case float64:
		return strconv.AppendFloat(nil, v, 'f', -1, 64), nil
	case bool:
		// 数字编码（"1"/"0"），与 writer 的 w.int(1)/w.int(0) 一致，
		// 不是 "true"/"false" 文本。
		if v {
			return []byte("1"), nil
		}
		return []byte("0"), nil
	case time.Time:
		return []byte(v.Format(time.RFC3339Nano)), nil
	case time.Duration:
		return strconv.AppendInt(nil, v.Nanoseconds(), 10), nil
	case net.IP:
		// 原始字节（4 或 16 字节），非点分文本；writer 的 net.IP case 同样
		// 直接 w.bytes(v)。
		return []byte(v), nil
	case encoding.BinaryMarshaler:
		return v.MarshalBinary()
	default:
		return nil, fmt.Errorf("redis: can't marshal %T (implement encoding.BinaryMarshaler)", v)
	}
}
