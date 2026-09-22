package redis

import "strings"

// isUnavailableText 判断错误文本是否表示 Redis 不可用。
// 文本判据集中维护在此，随 go-redis 版本漂移时只改此处。
func isUnavailableText(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "redis: connection pool timeout") ||
		strings.Contains(msg, "client is closed") ||
		strings.Contains(msg, "CLUSTERDOWN ")
}

// isUnknownCommandText 判断错误文本是否表示未知命令。
func isUnknownCommandText(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "ERR unknown")
}

// isNotAllowedText 判断错误文本是否表示权限不足。
func isNotAllowedText(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not allowed")
}

// isItemExistsText 判断错误文本是否表示已存在。
func isItemExistsText(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "item exists") ||
		strings.Contains(msg, "already exists")
}

// isNotFoundByText 判断错误文本是否表示未找到。
// 注意：大小写敏感，包含 "Not found" 与 "not found" 两种形式（实测结论）。
func isNotFoundByText(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Not found") ||
		strings.Contains(msg, "not found")
}
