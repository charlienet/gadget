package logger

import "testing"

// 多关键词交叠场景：较长词优先处理；与已收集区间相交的短词命中不重复替换；
// 相交短词的独立非重叠命中仍应打码（大小写不敏感匹配、原文切割）
func TestMaskTextOverlappingKeywords(t *testing.T) {
	tests := []struct {
		name string
		s    string
		keys []string
		mask string
		want string
	}{
		{
			name: "短词被长词完全包含则跳过",
			s:    "auth_token",
			keys: []string{"token", "auth_token"},
			mask: "X",
			want: "X",
		},
		{
			name: "嵌套部分相交不重复替换",
			s:    "ab_token",
			keys: []string{"token", "_token"},
			mask: "X",
			want: "abX",
		},
		{
			name: "长词掩后短词的独立命中仍打码",
			s:    "token and auth_token and token",
			keys: []string{"token", "auth_token"},
			mask: "*",
			want: "* and * and *",
		},
		{
			name: "大小写混合多处命中",
			s:    "TOKEN xtokenx",
			keys: []string{"token"},
			mask: "X",
			want: "X xXx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskText(tt.s, tt.keys, tt.mask); got != tt.want {
				t.Errorf("maskText(%q, %v, %q) = %q, want %q", tt.s, tt.keys, tt.mask, got, tt.want)
			}
		})
	}
}
