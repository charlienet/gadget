package cache

import (
	"slices"

	"github.com/bytedance/sonic"
)

type Serializer interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(b []byte, v any) error
}

// jsonSerializer 是默认序列化器，基于 bytedance/sonic（ConfigStd，行为与
// encoding/json 完全对齐：HTML 转义开启、map 键排序——缓存字节与旧实现一致）。
// []byte 裸存与 *string 特判分支保持原逻辑（不走 sonic）。
type jsonSerializer struct{}

func (jsonSerializer) Marshal(v any) ([]byte, error) {
	switch value := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return value, nil
	}

	// string 等其余类型统一走 sonic.ConfigStd.Marshal，保证序列化格式单一
	// （字符串输出为带引号的 JSON 字符串）。缓存数据重启即失，
	// 无需向后兼容旧格式。
	return sonic.ConfigStd.Marshal(v)
}

// unmarshalAny 将 data 经 s 解码为任意值（any），是 GetMulti 等「无已知目标类型」
// 读取路径的统一入口：与主读路径（respond → s.Unmarshal）、写入路径
// （Put/SetMulti → s.Marshal）共用同一 Serializer 编解码规则，消除编码不一致。
// 解码失败时回退 string(data)，保留对非法 JSON 数据（如 Marshal 对 []byte 裸存的
// 字节串）的兼容语义，保证存量数据读取行为不变。error 当前恒为 nil（失败已被回退
// 消化），签名保留以贴合 Go 惯例并为自定义 Serializer 实现留上报通道。
func unmarshalAny(s Serializer, data []byte) (any, error) {
	var v any
	if err := s.Unmarshal(data, &v); err != nil {
		return string(data), nil
	}
	return v, nil
}

func (jsonSerializer) Unmarshal(b []byte, v any) error {
	if len(b) == 0 {
		return nil
	}

	switch value := v.(type) {
	case nil:
		return nil
	case *[]byte:
		*value = slices.Clone(b)
		return nil
	case *string:
		// 优先按 JSON 字符串解码；失败（如 Marshal 对 []byte 裸存的数据）时
		// 回退为原始字节串，保证 Put([]byte) → Get(*string) 往返一致。
		if err := sonic.ConfigStd.Unmarshal(b, v); err != nil {
			*value = string(b)
		}
		return nil
	}

	return sonic.ConfigStd.Unmarshal(b, &v)
}
