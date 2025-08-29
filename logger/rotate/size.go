package rotate

import (
	"io"

	"gopkg.in/natefinch/lumberjack.v2"
)

func NewRotateSizeWriter(filename string, size, maxAge, maxBackups int) io.Writer {
	return &lumberjack.Logger{
		Filename:   filename,
		MaxSize:    size,
		MaxAge:     maxAge,
		MaxBackups: maxBackups,
		LocalTime:  true,
	}
}
