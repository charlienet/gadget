package logger

import (
	"context"
	"log/slog"
	"sort"
	"strings"
)

// 敏感信息过滤边界：
// 本包的敏感过滤（SensitiveOptions / WithSensitiveKeys）仅覆盖结构化字段
// （attrs / key=value 属性），消息文本（msg）不参与扫描——请勿在消息文本中携带
// 敏感信息（如密码、Token、密钥明文）。如需对消息文本打码，使用 SensitiveString。
//
// 掩码替换统一将敏感值替换为字符串掩码（slog.StringValue，默认 "******"），
// 原始类型信息丢失：JSON 下游（文件 handler）注意字段 schema 变化。

// SensitiveOptions 敏感信息过滤配置（应用端可配）
type SensitiveOptions struct {
	Keys  []string              // 用户追加敏感字段 key（子串匹配、大小写不敏感；与内置精确词集合并生效）
	Mask  string                // 掩码，默认 "******"
	Match func(key string) bool // 自定义匹配函数；非 nil 时优先于内置/用户词集匹配
}

// 内置默认敏感词集（兜底）：常见凭据字段名。
// 匹配策略为精确匹配（strings.EqualFold，大小写不敏感），避免误伤 secretary/tokenizer
// 等含敏感词前缀的合法字段；用户通过 WithSensitiveKeys 追加的词仍按子串匹配（显式指定即有意）。
var defaultSensitiveKeys = []string{
	"password", "passwd", "pwd", "secret", "token", "api_key", "apikey",
	"access_key", "accesskey", "authorization", "auth_token", "credential",
	"private_key", "privatekey", "refresh_token", "client_secret", "session_key",
	"salt", "cookie", "jwt", "x-api-key", "client_id", "session",
}

// sensitiveHandler 敏感信息过滤装饰器：Handle 时对敏感 key 的属性值打码后透传。
// 字段不可变（mask/match 构造时确定），WithAttrs/WithGroup 派生安全。
type sensitiveHandler struct {
	handler slog.Handler
	mask    string
	match   func(string) bool
}

// NewSensitiveHandler 包装 handler：Handle 时对敏感 key 的属性值打码后透传。
// opts 为 nil 或字段缺省时使用内置默认敏感词集（精确匹配）与默认掩码 "******"。
func NewSensitiveHandler(handler slog.Handler, opts *SensitiveOptions) slog.Handler {
	h := &sensitiveHandler{
		handler: handler,
		mask:    "******",
	}
	if opts != nil {
		if opts.Mask != "" {
			h.mask = opts.Mask
		}
		if opts.Match != nil {
			// 自定义匹配函数优先于词集匹配
			h.match = opts.Match
		} else {
			// 内置词集精确匹配 + 用户词子串匹配
			h.match = buildSensitiveMatcher(opts.Keys)
		}
	} else {
		h.match = buildSensitiveMatcher(nil)
	}

	return h
}

// buildSensitiveMatcher 构造匹配函数：
// 内置词集精确匹配（大小写不敏感，EqualFold 语义）+ 用户词子串匹配（大小写不敏感）
func buildSensitiveMatcher(userKeys []string) func(string) bool {
	builtin := make(map[string]struct{}, len(defaultSensitiveKeys))
	for _, k := range defaultSensitiveKeys {
		builtin[strings.ToLower(k)] = struct{}{}
	}

	users := make([]string, 0, len(userKeys))
	for _, k := range userKeys {
		if k != "" {
			users = append(users, strings.ToLower(k))
		}
	}

	return func(key string) bool {
		lk := strings.ToLower(key)
		if _, ok := builtin[lk]; ok {
			return true // 精确命中内置词
		}
		for _, kw := range users {
			if strings.Contains(lk, kw) {
				return true // 用户词子串命中
			}
		}

		return false
	}
}

// Enabled 透传底层 handler 的级别判断（slog 级别数值越小越详细）
func (h *sensitiveHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

// Handle 对敏感 key 的属性值打码后透传；无敏感字段时直接透传原 record。
// 有则重建（保留 PC）并替换敏感值为掩码，Group 属性递归处理。
func (h *sensitiveHandler) Handle(ctx context.Context, r slog.Record) error {
	if !recordHasSensitive(r, h.match) {
		return h.handler.Handle(ctx, r)
	}

	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(maskAttr(a, h.mask, h.match))
		return true
	})

	return h.handler.Handle(ctx, nr)
}

// recordHasSensitive 遍历 record 属性（含 Group 递归）判断是否存在敏感字段
func recordHasSensitive(r slog.Record, match func(string) bool) bool {
	has := false
	r.Attrs(func(a slog.Attr) bool {
		if attrHasSensitive(a, match) {
			has = true
			return false // 提前终止遍历
		}
		return true
	})

	return has
}

// attrHasSensitive 判断单个属性是否敏感；Group 递归检查内部属性
func attrHasSensitive(a slog.Attr, match func(string) bool) bool {
	if a.Value.Kind() == slog.KindGroup {
		for _, ga := range a.Value.Group() {
			if attrHasSensitive(ga, match) {
				return true
			}
		}
		return false
	}

	return match(a.Key)
}

// maskAttr 打码属性值；Group 递归处理内部属性。
// 敏感值统一替换为字符串掩码（slog.StringValue），原始类型信息丢失（见包注释）。
func maskAttr(a slog.Attr, mask string, match func(string) bool) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		inner := a.Value.Group()
		out := make([]slog.Attr, 0, len(inner))
		for _, ga := range inner {
			out = append(out, maskAttr(ga, mask, match))
		}

		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}

	if match(a.Key) {
		return slog.String(a.Key, mask)
	}

	return a
}

// SensitiveString 对消息文本按内置默认敏感词集做掩码替换（大小写不敏感），
// 供调用方对消息文本显式打码。
// 敏感过滤仅覆盖结构化字段，消息文本请勿携带敏感信息；若确需输出含敏感词的文本，
// 请先用本函数打码再记录。掩码为默认 "******"（如需自定义掩码请自行替换）。
func SensitiveString(s string) string {
	return maskText(s, defaultSensitiveKeys, "******")
}

// maskText 将文本中匹配 keys（大小写不敏感子串）的部分替换为 mask。
// 较长词优先处理，避免短词先命中破坏长词匹配（如 token 与 auth_token）。
func maskText(s string, keys []string, mask string) string {
	sorted := make([]string, len(keys))
	copy(sorted, keys)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })

	ls := strings.ToLower(s)
	for _, kw := range sorted {
		if kw == "" {
			continue
		}
		lk := strings.ToLower(kw)
		for {
			i := strings.Index(ls, lk)
			if i < 0 {
				break
			}
			s = s[:i] + mask + s[i+len(kw):]
			// 已替换区域以空格占位，保持后续索引与原文一致
			ls = ls[:i] + strings.Repeat(" ", len(kw)) + ls[i+len(kw):]
		}
	}
	return s
}

// WithAttrs 派生实例：预设属性同样打码后透传，避免 With 绕过过滤
func (h *sensitiveHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, maskAttr(a, h.mask, h.match))
	}

	return &sensitiveHandler{handler: h.handler.WithAttrs(out), mask: h.mask, match: h.match}
}

// WithGroup 透传底层（分组本身不参与匹配）
func (h *sensitiveHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	return &sensitiveHandler{handler: h.handler.WithGroup(name), mask: h.mask, match: h.match}
}
