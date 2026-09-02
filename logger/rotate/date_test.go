package rotate

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 所有用例的文件操作均限定在 t.TempDir()/t.Chdir 内，
// 测试目录与包源码目录不得残留产物（logs/、.hidefile*、裸日期文件）。

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "av.log")

	w := &RotateDateWriter{Filename: filename}
	defer w.Close()

	for i := range 100 {
		if _, err := w.Write([]byte("abc" + strconv.Itoa(i) + "\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "av."+time.Now().Format("2006-01-02")+".log"))
	if err != nil {
		t.Fatalf("read dated file: %v", err)
	}
	if !strings.Contains(string(data), "abc42") || !strings.HasSuffix(string(data), "abc99\n") {
		t.Errorf("expected all 100 lines in dated file, got: %q", data)
	}
}

func TestMuti(t *testing.T) {
	dir := t.TempDir()
	w := &RotateDateWriter{Filename: filepath.Join(dir, "av.test.log")}
	defer w.Close()

	for i := range 10 {
		if _, err := w.Write([]byte("abc" + strconv.Itoa(i) + "\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	// 多扩展名段：只切最后一段 .log，日期插在中间
	if _, err := os.Stat(filepath.Join(dir, "av.test."+time.Now().Format("2006-01-02")+".log")); err != nil {
		t.Errorf("expected mid-dated filename: %v", err)
	}
}

func TestNoExt(t *testing.T) {
	dir := t.TempDir()
	w := &RotateDateWriter{Filename: filepath.Join(dir, "av")}
	defer w.Close()

	if _, err := w.Write([]byte("noext")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "av."+time.Now().Format("2006-01-02"))); err != nil {
		t.Errorf("expected no-ext dated file: %v", err)
	}
}

func TestHideFile(t *testing.T) {
	dir := t.TempDir()
	w := &RotateDateWriter{Filename: filepath.Join(dir, ".hidefile")}
	defer w.Close()

	if _, err := w.Write([]byte("hidefile")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 隐藏文件：日期后缀直接追加在文件名后
	if _, err := os.Stat(filepath.Join(dir, ".hidefile."+time.Now().Format("2006-01-02"))); err != nil {
		t.Errorf("expected hidden dated file: %v", err)
	}
}

func TestNoFileName(t *testing.T) {
	// Filename 为空：日期文件落在当前工作目录——chdir 进 TempDir 防产物残留
	t.Chdir(t.TempDir())

	w := &RotateDateWriter{}
	defer w.Close() //nolint:errcheck

	if _, err := w.Write([]byte("abc11")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(time.Now().Format("2006-01-02")); err != nil {
		t.Errorf("expected bare date file in cwd: %v", err)
	}
}

func BenchmarkWrite(b *testing.B) {
	dir := b.TempDir()
	w := &RotateDateWriter{Filename: filepath.Join(dir, "bench.log")}
	defer w.Close()

	for i := 0; b.Loop(); i++ {
		_, _ = w.Write([]byte("abc" + strconv.Itoa(i) + "\n"))
	}
}
