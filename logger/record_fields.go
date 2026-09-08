package logger

import (
	"log/slog"
)

// record_fields.go：console 与 fileText 两个 handler 共享的「字段顺序语义」单一事实来源。
//
// 两者的字节级渲染风格不同（console：ANSI 颜色、`[LEVEL]`、字符串不加引号；
// fileText：无颜色、`level=INFO`、k=v、按标准 quoting），故不共享渲染，只共享：
//  1. 从 record 顶层挑选 trace_id/req_id 的判据（scanTraceIDs）；
//  2. 把已前置输出的 trace_id/req_id 从「其余 attrs」中剔除的判据（isTraceIDAttr）；
//  3. 字段插入位置约定：trace_id/req_id 一律排在 level 之后、msg 之前；
//     source（若启用）排在 msg 之后、其余 attrs 之前。
//
// 顺序（两 handler 一致）：time → level → trace_id(如有) → req_id(如有) → msg → source(可选) → 其余 attrs。
//
// 注意：trace_id/req_id 由最外层 TraceHandler 用 r.AddAttrs 追加到 record 自身 attrs，
// 故二者只从 record 顶层扫描；handler 的 WithAttrs 累积属性（h.attrs）不参与该挑选
// （保持既有语义，With 预设的 trace_id 仍按普通属性输出）。

// scanTraceIDs 遍历 record 顶层属性，提取 trace_id / req_id 值（仅精确 key、非 Group、
// Kind 为 String）。Record.Attrs 可安全重复遍历，故此处仅作扫描不缓冲其余属性。
// console 与 fileText 共享此判据。
func scanTraceIDs(r slog.Record) (traceID, reqID string) {
	r.Attrs(func(a slog.Attr) bool {
		if a.Value.Kind() == slog.KindGroup {
			return true
		}
		if a.Value.Kind() != slog.KindString {
			return true
		}
		switch a.Key {
		case AttrTraceID:
			traceID = a.Value.String()
		case AttrReqID:
			reqID = a.Value.String()
		}
		return true
	})

	return traceID, reqID
}

// isTraceIDAttr 判断某 record 属性是否为已前置输出的 trace_id/req_id（需从其余 attrs 剔除，
// 避免 msg 之后重复）。判据与 scanTraceIDs 保持一致：顶层、Kind 为 String、精确 key，
// 且对应值已被提取（非空）。console 与 fileText 共享此判据。
func isTraceIDAttr(a slog.Attr, traceID, reqID string) bool {
	if a.Value.Kind() == slog.KindGroup || a.Value.Kind() != slog.KindString {
		return false
	}
	if traceID != "" && a.Key == AttrTraceID {
		return true
	}
	if reqID != "" && a.Key == AttrReqID {
		return true
	}

	return false
}
