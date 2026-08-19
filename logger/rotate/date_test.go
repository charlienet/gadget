package rotate

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestWrite(t *testing.T) {
	w := &RotateDateWriter{Filename: "logs/av.log"}
	defer w.Close() // 释放文件句柄

	for i := range 100 {
		_, _ = w.Write([]byte("abc" + strconv.Itoa(i) + "\n"))
	}
}

func TestMuti(t *testing.T) {
	w := &RotateDateWriter{Filename: "logs/av.test.log"}
	defer w.Close()

	for i := range 10 {
		_, _ = w.Write([]byte("abc" + strconv.Itoa(i) + "\n"))
	}
}

func TestNoExt(t *testing.T) {
	w := &RotateDateWriter{Filename: "logs/av"}
	defer w.Close()
	_, _ = w.Write([]byte("noext"))
}

func TestHideFile(t *testing.T) {
	w := &RotateDateWriter{Filename: ".hidefile"}
	defer w.Close()
	_, _ = w.Write([]byte("hidefile"))
}

func TestNoFileName(t *testing.T) {
	w := &RotateDateWriter{}

	_, _ = w.Write([]byte("abc11"))
}

// TestDateRotateCleanup 验证 MaxAge 清理：超过保留天数的历史日期文件被删除，
// 保留期内的文件不受影响（直接调用内部清理逻辑，时间可控）。
func TestDateRotateCleanup(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "app.log")
	w := &RotateDateWriter{Filename: filename, Layout: "2006-01-02", MaxAge: 3}
	defer w.Close()

	layout := "2006-01-02"
	now := time.Now()
	// 构造 5 个历史日期文件：今天、-1、-2、-4、-5 天（MaxAge=3 时 -4/-5 应被清理）
	dates := []time.Time{now, now.AddDate(0, 0, -1), now.AddDate(0, 0, -2), now.AddDate(0, 0, -4), now.AddDate(0, 0, -5)}
	for _, d := range dates {
		p := filepath.Join(dir, dateFilename("app.log", d.Format(layout)))
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("failed to create file: %v", err)
		}
	}

	if err := w.cleanupOld(); err != nil {
		t.Fatalf("cleanupOld failed: %v", err)
	}

	for _, d := range dates {
		p := filepath.Join(dir, dateFilename("app.log", d.Format(layout)))
		age := int(now.Sub(d).Hours() / 24)
		_, err := os.Stat(p)
		if age >= 4 {
			if err == nil {
				t.Errorf("expected stale file removed: %s", p)
			}
		} else if err != nil {
			t.Errorf("expected recent file kept: %s", p)
		}
	}
}

// TestDateRotateCompress 验证 Compress：对上一日文件 gzip 压缩后删除原文件（标准库 compress/gzip）
func TestDateRotateCompress(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "app.log")
	w := &RotateDateWriter{Filename: filename, Layout: "2006-01-02"}

	date := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	raw := filepath.Join(dir, dateFilename("app.log", date))
	if err := os.WriteFile(raw, []byte("compress me"), 0644); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}

	if err := w.compressFile(date); err != nil {
		t.Fatalf("compressFile failed: %v", err)
	}

	if _, err := os.Stat(raw); !os.IsNotExist(err) {
		t.Errorf("expected raw file removed after compress, got err=%v", err)
	}

	data, err := os.ReadFile(raw + ".gz")
	if err != nil {
		t.Fatalf("failed to read gz file: %v", err)
	}
	// 验证 gzip 魔数 0x1f 0x8b
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		t.Errorf("expected gzip magic bytes, got %v", data[:2])
	}
}

// TestDateRotateCleanupGz 验证压缩后的 .gz 历史文件同样参与 MaxAge 清理
func TestDateRotateCleanupGz(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "app.log")
	w := &RotateDateWriter{Filename: filename, Layout: "2006-01-02", MaxAge: 3}

	layout := "2006-01-02"
	old := time.Now().AddDate(0, 0, -5).Format(layout)
	gz := filepath.Join(dir, dateFilename("app.log", old)+".gz")
	if err := os.WriteFile(gz, []byte("x"), 0644); err != nil {
		t.Fatalf("failed to create gz file: %v", err)
	}

	if err := w.cleanupOld(); err != nil {
		t.Fatalf("cleanupOld failed: %v", err)
	}

	if _, err := os.Stat(gz); !os.IsNotExist(err) {
		t.Errorf("expected stale gz file removed, got err=%v", err)
	}
}

func BenchmarkWrite(b *testing.B) {
	w := &RotateDateWriter{Filename: "logs/aaaa.log"}
	defer w.Close()

	for i := 0; b.Loop(); i++ {
		_, _ = w.Write([]byte("abc" + strconv.Itoa(i) + "\n"))
	}
}
