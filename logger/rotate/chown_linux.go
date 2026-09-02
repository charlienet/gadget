package rotate

import (
	"os"
	"syscall"
)

// osChown is a var so we can mock it out during tests.
var osChown = os.Chown

// chown 将 name 的属主对齐到 info（Stat 结果）描述的属主。
// 调用前提：文件已存在（os.Stat 成功）。禁止在此重开文件——
// O_TRUNC 会把当日已有日志静默清空（同日进程重启即丢数据，B-1 事故根因）。
// lumberjack 原版只在旧文件已 rename 走、目标不存在时才调用 chown，
// 本包的调用条件不同，直接对已存在文件设置属主即可。
func chown(name string, info os.FileInfo) error {
	stat := info.Sys().(*syscall.Stat_t)
	return osChown(name, int(stat.Uid), int(stat.Gid))
}
