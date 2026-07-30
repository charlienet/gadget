package rotate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewRotateSizeWriter(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "test.log")

	w := NewRotateSizeWriter(filename, 1, 3, 2)
	if w == nil {
		t.Fatal("NewRotateSizeWriter returned nil")
	}

	n, err := w.Write([]byte("hello\n"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if n != 6 {
		t.Errorf("expected 6 bytes written, got %d", n)
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("expected 'hello\\n', got %q", string(data))
	}
}

func TestNewRotateSizeWriterMultipleWrites(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "multi.log")

	w := NewRotateSizeWriter(filename, 10, 3, 5)
	for i := range 3 {
		_, err := w.Write([]byte("line\n"))
		if err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if len(data) < 3*5 {
		t.Errorf("expected at least %d bytes, got %d", 3*5, len(data))
	}
}
