package cache

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGetMultiUsesSerializerConsistently 回归：GetMulti 与 Put/单键主读路径统一
// 经同一 Serializer 编解码（不再直接调用标准库 encoding/json），三种典型数据往返正确：
//   - 结构体 → map[string]any（字段可取回）
//   - []byte 裸值（Marshal 裸存、非法 JSON）→ 回退为原字符串且不报错
//   - 字符串 → string
func TestGetMultiUsesSerializerConsistently(t *testing.T) {
	c := New(WithMemStore())
	defer c.Close()
	ctx := context.Background()

	type item struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	assert.Nil(t, c.Put(ctx, "struct", item{Name: "gadget", Age: 3}, 60))
	assert.Nil(t, c.Put(ctx, "raw", []byte("rawdata"), 60))
	assert.Nil(t, c.Put(ctx, "str", "hello", 60))

	res, err := c.GetMulti(ctx, "struct", "raw", "str")
	assert.Nil(t, err)
	assert.Len(t, res, 3)

	// 结构体经 Serializer 解码为 map[string]any，字段可取回
	m, ok := res["struct"].(map[string]any)
	assert.True(t, ok, "struct 应解码为 map[string]any，实际 %T", res["struct"])
	if ok {
		assert.Equal(t, "gadget", m["name"])
		assert.Equal(t, float64(3), m["age"])
	}

	// []byte 裸值无法 JSON 解码，回退原始字符串且不报错
	assert.Equal(t, "rawdata", res["raw"])

	// 字符串往返一致
	assert.Equal(t, "hello", res["str"])
}
