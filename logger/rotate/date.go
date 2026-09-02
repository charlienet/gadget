package rotate

import (
	"compress/gzip"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RotateDateWriter 按日期轮换的文件 writer：切换日期时自动滚动到新文件。
// Compress=true 时对上一日文件 gzip 压缩（压缩后删除原文件）；
// MaxAge>0 时清理超过保留天数的历史日期文件（含 .gz）。
// 跨日压缩与过期清理在后台 goroutine 串行执行（m-1，对齐 lumberjack 做法），
// 不阻塞午夜后首条日志；失败经 slog.Warn 报告（见 rotate 内注释）。
type RotateDateWriter struct {
	Filename string
	Layout   string
	Compress bool
	MaxAge   int // 保留天数（<=0 不清理历史文件）

	time string
	file *os.File
	mu   sync.Mutex

	// 后台维护任务（m-1）：
	// cleanupMu 串行化压缩/清理任务，跨多个切换日也不会并发操作文件；
	// pending 供 Close 等待未完成任务（生命周期策略：Close 阻塞至后台任务收敛，
	// 返回前产物已压缩/清理或已 Warn 上报；Write 内的跨日快路径不等待。
	// NEW-1：任务只在新日期文件成功打开后派发——rotate 失败零派发零 Warn，
	// 杜绝「Warn 自路由写回故障 writer → 再派发」的自反馈风暴）。
	cleanupMu sync.Mutex
	pending   sync.WaitGroup
}

func (r *RotateDateWriter) Write(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.Layout == "" {
		r.Layout = "2006-01-02"
	}

	date := time.Now().Format(r.Layout)
	if r.file == nil || r.time != date {
		if err := r.rotate(date); err != nil {
			return 0, err
		}
	}

	return r.file.Write(p)
}

// rotate 切换到新日期文件：先关闭当前文件；发生日期切换时，在**新日期文件成功
// 打开之后**才把上一日文件的压缩/清理派发到后台（m-1 + NEW-1：既避开 Write
// 锁路径的午夜阻塞，又保证只在 writer 健康时派发）；打开失败则不派发、返回错误。
// 调用方必须持有 r.mu（Write / Close / 测试）。
func (r *RotateDateWriter) rotate(date string) error {
	oldDate := r.time
	if err := r.close(); err != nil {
		return err
	}

	dir := filepath.Dir(r.Filename)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}

	filename := r.filename(date)
	fullpath := filepath.Join(dir, filename)

	info, err := os.Stat(fullpath)
	if err == nil {
		if err := chown(fullpath, info); err != nil {
			return err
		}
	}

	file, err := os.OpenFile(fullpath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return err
	}

	r.file = file

	// 日期切换（非首次写入）时压缩上一日文件 + 按 MaxAge 清理过期历史文件。
	// 大文件 gzip 可达秒级，同步执行会阻塞跨日后首条日志（午夜效应）——移入
	// 后台 goroutine，配置以值快照传参（goroutine 不回读 r 的公开字段，避免与
	// 调用方改配置产生数据竞争）。
	//
	// NEW-1：派发点必须在「新文件成功打开」之后，这是自反馈循环的安全依据：
	//   - rotate 失败（目录被运行期破坏 / 权限变更 / NFS ESTALE）→ 不派发 →
	//     无 Warn → 退化为与旧同步实现一致的「Write 返错」，无重试风暴；
	//   - rotate 成功 → writer 健康，任务失败的 slog.Warn 即使经默认 logger
	//     自路由写回本 writer（New(WithFile(...WithDateRotate)) 内部 SetDefault
	//     的真实形态），Write 也命中「file 非 nil 且日期一致」快路径，不触发
	//     新 rotate/新派发，链长恒为 1。
	//     （派发先于打开的旧版本曾实证：Warn→写回→再派发 → ~15 万次/秒
	//     goroutine 风暴且 pending 永不收敛、Close 永久挂起。）
	if oldDate != "" && oldDate != date {
		compress, maxAge, layout, logFile := r.Compress, r.MaxAge, r.Layout, r.Filename
		r.pending.Add(1)
		go func(old string) {
			defer r.pending.Done()
			// 串行化：多个跨日任务也不会并发压缩 / 交叉删除同一目录文件
			r.cleanupMu.Lock()
			defer r.cleanupMu.Unlock()

			if compress {
				if err := compressFileAt(logFile, old); err != nil {
					slog.Warn("rotate: compress previous date file failed",
						"filename", logFile, "date", old, "err", err)
				}
			}
			if err := cleanupOldFiles(logFile, layout, maxAge); err != nil {
				slog.Warn("rotate: cleanup old date files failed",
					"filename", logFile, "err", err)
			}
		}(oldDate)
	}

	return nil
}

// filename 返回指定日期对应的文件名（带副作用：记录 r.time=date）
func (r *RotateDateWriter) filename(date string) string {
	defer func() {
		r.time = date
	}()

	return dateFilename(r.Filename, date)
}

// dateFilename 计算日期轮换文件名（纯函数，无副作用）：
// 隐藏文件 ".x" → ".x.date"；普通文件 "a.log" → "a.date.log"；
// 无扩展名 "a" → "a.date"；空 Filename → date
func dateFilename(filename, date string) string {
	if filename == "" {
		return date
	}

	filename = filepath.Base(filename)
	if strings.HasPrefix(filename, ".") {
		return filename + "." + date
	}

	ext := filepath.Ext(filename)
	name := filename[:len(filename)-len(ext)]

	return name + "." + date + ext
}

// compressFile 对指定日期的文件做 gzip 压缩（同步薄包装，读 r.Filename；
// 跨日后台路径用 compressFileAt 值快照，见 rotate）。文件不存在时静默跳过。
func (r *RotateDateWriter) compressFile(date string) error {
	return compressFileAt(r.Filename, date)
}

// compressFileAt 对 filename 指定日期的文件做 gzip 压缩（标准库 compress/gzip，
// 压缩后删除原文件）。文件不存在时静默跳过（可能已被压缩或清理）。
func compressFileAt(filename, date string) error {
	full := filepath.Join(filepath.Dir(filename), dateFilename(filename, date))
	if _, err := os.Stat(full); err != nil {
		return nil
	}

	src, err := os.Open(full)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(full + ".gz")
	if err != nil {
		return err
	}

	zw := gzip.NewWriter(dst)
	_, err = io.Copy(zw, src)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	// Windows 上删除文件前必须已关闭其句柄：先关源文件再删除（defer src.Close 兜底）
	src.Close()
	if err != nil {
		return err
	}

	return os.Remove(full)
}

// cleanupOld 按 r 的配置清理过期历史文件（同步薄包装；跨日后台路径见 rotate）。
func (r *RotateDateWriter) cleanupOld() error {
	return cleanupOldFiles(r.Filename, r.Layout, r.MaxAge)
}

// cleanupOldFiles 扫描目录中匹配日期命名模式的文件，删除超过 maxAge 天的历史文件。
// 匹配规则：文件名 = 基准名 + "." + 日期 + 扩展名（含 .gz），日期部分按 layout 解析，
// 解析失败的文件（非日期命名）跳过。收集后统一删除（避免遍历中删除目录项）。
// maxAge <= 0 不清理，直接返回 nil。
//
// 当前文件安全不变式（m-1 后台化的前提）：清理只删除解析日期早于
// now-maxAge 的文件，而正在写入的当前文件日期 = now（按 layout），恒不早于
// cutoff，故后台执行晚于新文件打开也不会误删当前日志。
func cleanupOldFiles(filename, layout string, maxAge int) error {
	if maxAge <= 0 {
		return nil
	}

	dir := filepath.Dir(filename)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	base, ext := dateFilePattern(filename)
	cutoff := time.Now().AddDate(0, 0, -maxAge)

	var stale []string
	for _, e := range entries {
		name := e.Name()
		if base != "" && !strings.HasPrefix(name, base) {
			continue
		}
		// 允许 .gz 后缀：压缩后的历史文件同样参与清理
		if before, ok := strings.CutSuffix(name, ".gz"); ok {
			name = before
		}
		if !strings.HasSuffix(name, ext) {
			continue
		}

		mid := strings.TrimSuffix(strings.TrimPrefix(name, base), ext)
		if mid == "" {
			continue
		}

		d, err := time.ParseInLocation(layout, mid, time.Local)
		if err != nil {
			continue // 非日期命名文件，跳过
		}
		if d.Before(cutoff) {
			stale = append(stale, filepath.Join(dir, e.Name()))
		}
	}

	for _, p := range stale {
		_ = os.Remove(p)
	}
	return nil
}

// dateFilePattern 返回日期文件的基准前缀与扩展名（用于目录扫描匹配）。
// "a.log" → ("a.", ".log")；".hide" → (".hide.", "")；"" → ("", "")。
// 注意：空判断必须在 filepath.Base 之前——Base("") 返回 "."，
// 后置判断不可达且会把空输入误算成 ("..", "")。
func dateFilePattern(filename string) (base, ext string) {
	if filename == "" {
		return "", ""
	}
	filename = filepath.Base(filename)
	if strings.HasPrefix(filename, ".") {
		return filename + ".", ""
	}
	ext = filepath.Ext(filename)

	return filename[:len(filename)-len(ext)] + ".", ext
}

// Close 关闭 writer。生命周期策略（m-1）：先等待跨日后台压缩/清理任务全部完成
// 再释放句柄——返回时后台产物已落定（.gz / 删除 / Warn 上报），不遗留半途任务。
// 等待期间不持 r.mu（后台任务不依赖 mu，避免无谓串行）。
//
// NEW-2（超时预算）：pending.Wait 无自有超时——病态文件系统（如 NFS 挂起时
// gzip/cleanup 卡死）下 Close 可能长时间阻塞，超出上层 logger.Close(timeout)
// 的预算；对退出时延有硬约束的部署应确保存储健康或关闭 Compress/MaxAge 清理。
//
// NEW-3（Add-after-Wait 窗口）：Wait 与并发跨日 Write 之间存在窗口——Wait 返回后，
// 迟到的跨日 Write 仍可派发新任务并在 Close 返回后继续运行。后果有界：原始文件
// 的删除是压缩的最后一步，进程此刻退出最多残留半截 .gz（可弃、无数据丢失），
// 原始旧文件仍在。Close 应与停止业务写入配对使用（进程退出前调用）。
func (r *RotateDateWriter) Close() error {
	r.pending.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.close()
}

// close 关闭当前文件句柄。无论关闭成败都置 r.file = nil：
// 失败时若保留坏句柄，r.time 仍等于当前日期使 Write 跳过 rotate，
// 后续所有写入将永久报 "file already closed"（自中毒）。
// 置 nil 后下一次 Write 会重新走 rotate 打开路径，故障可自愈。
// 返回首次关闭失败的错误，供上层聚合。
func (r *RotateDateWriter) close() error {
	if r.file != nil {
		f := r.file
		r.file = nil
		return f.Close()
	}

	return nil
}
