package rotate

import (
	"io"

	"gopkg.in/natefinch/lumberjack.v2"
)

// NewRotateSizeWriter 创建按大小轮换的文件 writer（lumberjack）。
// compress 控制轮换后旧文件是否 gzip 压缩（lumberjack.Logger.Compress）。
// 返回 io.WriteCloser：调用方负责在退出时 Close 释放文件句柄。
func NewRotateSizeWriter(filename string, size, maxAge, maxBackups int, compress bool) io.WriteCloser {
	return &lumberjack.Logger{
		Filename:   filename,
		MaxSize:    size,
		MaxAge:     maxAge,
		MaxBackups: maxBackups,
		Compress:   compress,
		LocalTime:  true,
	}
}
