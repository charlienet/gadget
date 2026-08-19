package rotate

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RotateDateWriter 按日期轮换的文件 writer：切换日期时自动滚动到新文件。
// Compress=true 时对上一日文件 gzip 压缩（压缩后删除原文件）；
// MaxAge>0 时清理超过保留天数的历史日期文件（含 .gz）。
type RotateDateWriter struct {
	Filename string
	Layout   string
	Compress bool
	MaxAge   int // 保留天数（<=0 不清理历史文件）

	time string
	file *os.File
	mu   sync.Mutex
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

// rotate 切换到新日期文件：先关闭当前文件；发生日期切换时压缩/清理上一日文件，
// 最后打开（或复用）新日期文件。
func (r *RotateDateWriter) rotate(date string) error {
	oldDate := r.time
	if err := r.close(); err != nil {
		return err
	}

	// 日期切换（非首次写入）时：压缩上一日文件 + 按 MaxAge 清理过期历史文件
	if oldDate != "" && oldDate != date {
		if r.Compress {
			if err := r.compressFile(oldDate); err != nil {
				return err
			}
		}
		if err := r.cleanupOld(); err != nil {
			return err
		}
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

// compressFile 对指定日期的文件做 gzip 压缩（标准库 compress/gzip，压缩后删除原文件）。
// 文件不存在时静默跳过（可能已被压缩或清理）。
func (r *RotateDateWriter) compressFile(date string) error {
	full := filepath.Join(filepath.Dir(r.Filename), dateFilename(r.Filename, date))
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

// cleanupOld 扫描目录中匹配日期命名模式的文件，删除超过 MaxAge 天的历史文件。
// 匹配规则：文件名 = 基准名 + "." + 日期 + 扩展名（含 .gz），日期部分按 Layout 解析，
// 解析失败的文件（非日期命名）跳过。收集后统一删除（避免遍历中删除目录项）。
func (r *RotateDateWriter) cleanupOld() error {
	if r.MaxAge <= 0 {
		return nil
	}

	dir := filepath.Dir(r.Filename)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	base, ext := dateFilePattern(r.Filename)
	cutoff := time.Now().AddDate(0, 0, -r.MaxAge)

	var stale []string
	for _, e := range entries {
		name := e.Name()
		if base != "" && !strings.HasPrefix(name, base) {
			continue
		}
		// 允许 .gz 后缀：压缩后的历史文件同样参与清理
		if strings.HasSuffix(name, ".gz") {
			name = strings.TrimSuffix(name, ".gz")
		}
		if !strings.HasSuffix(name, ext) {
			continue
		}

		mid := strings.TrimSuffix(strings.TrimPrefix(name, base), ext)
		if mid == "" {
			continue
		}

		d, err := time.ParseInLocation(r.Layout, mid, time.Local)
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
// "a.log" → ("a.", ".log")；".hide" → (".hide.", "")；"" → ("", "")
func dateFilePattern(filename string) (base, ext string) {
	filename = filepath.Base(filename)
	if filename == "" {
		return "", ""
	}
	if strings.HasPrefix(filename, ".") {
		return filename + ".", ""
	}
	ext = filepath.Ext(filename)

	return filename[:len(filename)-len(ext)] + ".", ext
}

func (r *RotateDateWriter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.close()
}

func (r *RotateDateWriter) close() error {
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			return err
		}
		r.file = nil
	}

	return nil
}
