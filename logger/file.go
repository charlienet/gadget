package logger

import (
	"io"

	"github.com/charlienet/gadget/logger/rotate"
)

// defaultFileOptions 返回默认文件轮换配置
func defaultFileOptions() *FileOptions {
	return &FileOptions{
		MaxSize:    100,
		MaxAge:     30,
		MaxBackups: 10,
		Compress:   true,
	}
}

// buildFileWriter 构建文件 writer：
// Layout 非空用 rotate.RotateDateWriter（按日期轮换），否则按大小轮换（lumberjack）。
// 返回 io.WriteCloser：调用方负责在进程退出（包级 Close）时关闭以释放句柄。
func buildFileWriter(path string, fo *FileOptions) io.WriteCloser {
	if fo == nil {
		fo = defaultFileOptions()
	}

	if fo.Layout != "" {
		return &rotate.RotateDateWriter{
			Filename: path,
			Layout:   fo.Layout,
			Compress: fo.Compress,
			MaxAge:   fo.MaxAge, // 日期模式保留天数（<=0 不清理）
		}
	}

	return rotate.NewRotateSizeWriter(path, fo.MaxSize, fo.MaxAge, fo.MaxBackups, fo.Compress)
}
