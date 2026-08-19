package logger

import (
	"io"
	"os"
)

// ShouldColor 判断是否应该启用终端彩色输出
// 默认启用，通过 NO_COLOR 环境变量禁用（https://no-color.org/）
func ShouldColor() bool {
	return os.Getenv("NO_COLOR") == ""
}

// IsTerminal 判断 writer 是否为终端（TTY）
// 管道/文件输出时返回 false，用于自动去除 ANSI 颜色码，避免日志污染
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return stat.Mode()&os.ModeCharDevice != 0
}
