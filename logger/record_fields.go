package logger

import (
	"log/slog"
)

// record_fields.go：console 与 fileText 两个 handler 共享的「字段顺序语义」单一事实来源。
//
// 两者的字节级渲染风格不同（console：ANSI 颜色、`[LEVEL]`、字符串不加引号；
// fileText：无颜色、`level=INFO`、k=v、按标准 quoting），故不共享渲染，只共享：
//  1. 前置字段集合与相对次序（frontFieldKeys：service → env → trace_id → req_id）；
//  2. 从 record 顶层与 handler 累积 attrs（h.attrs）双源挑选前置字段的判据（pickFrontFields）；
//  3. 把已前置输出的字段从「其余 attrs」中剔除的判据（isPickedFrontAttr）；
//  4. 字段插入位置约定：前置字段一律排在 level 之后、msg 之前；
//     source（若启用）排在 msg 之后、其余 attrs 之前。
//
// 顺序（两 handler 一致）：
//
//	time → level → service(如有) → env(如有) → trace_id(如有) → req_id(如有) → msg → source(可选) → 其余 attrs
//
// 挑选规则（双源，两个 handler 一致；四个 key 各自独立判定）：
//   - record 顶层优先于 h.attrs（ctx 注入是请求级最新事实）：两源并存时前置 record 的值，
//     h.attrs 中同名项被去重剔除；
//   - h.attrs 内同一 key 出现多次时取最后一次出现的值，其余全部剔除（不改动 WithAttrs 的
//     append 行为，双源挑选仅对前置字段取最后一次出现的值并剔除重复）；若最后一次出现的值
//     为空串，则不触发前置与剔除，同名项均按普通属性输出（自洽退化）；
//   - h.groups 非空时 h.attrs 全部视为带前缀，不参与前置挑选（与 record 内 Group 不命中对称）；
//   - 两来源判据一致：顶层、非 Group、Kind 为 String 才命中（值为空串时不输出，
//     与 TraceHandler 的 ctx 空值过滤一致；即 record 存在但值为空串时不阻断 h.attrs 值前置）。
//
// 注：JSON 路径（slog.NewJSONHandler）保持标准语义，不经过本文件的挑选/去重逻辑。

// frontFieldKeys 是需要前置到 msg 之前的字段 key，按渲染相对次序排列（缺失即跳过）。
// 两个 handler 依此顺序输出，是「顺序语义」的单一事实来源。
// 包级只读共享，渲染按此序遍历，禁止 append/就地修改。
var frontFieldKeys = []string{AttrService, AttrEnv, AttrTraceID, AttrReqID}

// frontFields 是双源挑选后的前置字段结果（key → 值）。值为空串表示该 key 未命中
// （不前置、不触发去重）。渲染须按 frontFieldKeys 的固定次序取值，保证两 handler 一致。
type frontFields map[string]string

// get 返回某前置字段的挑选值（未命中为空串）。
func (f frontFields) get(key string) string { return f[key] }

// captureFrontAttr 单条 attr 的前置字段判据：仅顶层、非 Group、Kind 为 String、且 key 属于
// frontFieldKeys（out 预置了全部前置 key，故以 out 为准）才写入 out，同名覆盖 → 取最后一次出现
// 的值（含空串，末次覆盖前次）。scanFrontFields（attrs 切片）与 scanRecordFrontFields（record
// 遍历）共用此判据，避免两处复制。frontFields 是 map 引用，就地修改，返回无意义。
func captureFrontAttr(out frontFields, a slog.Attr) {
	if a.Value.Kind() == slog.KindGroup || a.Value.Kind() != slog.KindString {
		return
	}
	if _, ok := out[a.Key]; ok {
		out[a.Key] = a.Value.String()
	}
}

// scanFrontFields 从 attrs 切片（h.attrs）提取各前置字段值（判据见 captureFrontAttr）。
// 返回的 map 覆盖全部 frontFieldKeys，未命中者值为空串。
func scanFrontFields(attrs []slog.Attr) frontFields {
	out := newFrontFields()
	for _, a := range attrs {
		captureFrontAttr(out, a)
	}

	return out
}

// newFrontFields 构造以全部 frontFieldKeys 为键、值空串的挑选结果容器。
func newFrontFields() frontFields {
	out := make(frontFields, len(frontFieldKeys))
	for _, k := range frontFieldKeys {
		out[k] = ""
	}

	return out
}

// scanRecordFrontFields 遍历 record 顶层属性挑选前置字段（判据同 captureFrontAttr：
// 顶层、非 Group、KindString、同名取最后一次）。Record.Attrs 可安全重复遍历。
func scanRecordFrontFields(r slog.Record) frontFields {
	out := newFrontFields()
	r.Attrs(func(a slog.Attr) bool {
		captureFrontAttr(out, a)
		return true
	})

	return out
}

// pickFrontFields 从 record 顶层与 handler 累积 attrs 双源挑选前置字段
// （service/env/trace_id/req_id）。record 命中（非空串）优先；h.attrs 同名取最后一次出现；
// groups 非空时 h.attrs 全部视为带前缀、不参与挑选；两来源均仅非 Group、KindString 才命中。
//
// 合并语义：先按 record 扫得结果；groups 为空时，对 record 值为空串的 key 用 h.attrs 值顶位
// ——值空串视为未命中（与 TraceHandler 的 ctx 空值过滤一致），故 record 存在该 key 但值为
// 空串时，不阻断 h.attrs 值前置。返回 map 中值为空串的 key 表示最终未命中（不前置）。
func pickFrontFields(r slog.Record, attrs []slog.Attr, groups []string) frontFields {
	picked := scanRecordFrontFields(r)
	if len(groups) > 0 {
		return picked
	}

	attrVals := scanFrontFields(attrs)
	for _, key := range frontFieldKeys {
		if picked[key] == "" {
			picked[key] = attrVals[key]
		}
	}

	return picked
}

// isPickedFrontAttr 判断某个 attr 是否为已前置输出的前置字段（需从其余 attrs 剔除，避免
// msg 之后重复）。判据与 pickFrontFields 保持一致：非 Group、Kind 为 String、key 属于前置
// 字段集合，且该 key 已被挑选（值非空）。prefixed 表示该 attr 渲染时带分组前缀
// （h.groups 非空）——此时其 key 与前置的裸 key 不同名，不剔除。console 与 fileText 共享此判据。
func isPickedFrontAttr(a slog.Attr, picked frontFields, prefixed bool) bool {
	if prefixed || a.Value.Kind() == slog.KindGroup || a.Value.Kind() != slog.KindString {
		return false
	}

	return picked[a.Key] != ""
}
