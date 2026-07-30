package rotate

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RotateDateWriter struct {
	Filename string
	Layout   string
	Compress bool

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

func (r *RotateDateWriter) rotate(date string) error {
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
	return nil
}

func (r *RotateDateWriter) filename(date string) string {
	defer func() {
		r.time = date
	}()

	if r.Filename == "" {
		return date
	}

	filename := filepath.Base(r.Filename)
	if strings.HasPrefix(filename, ".") {
		return filename + "." + date
	}

	ext := filepath.Ext(r.Filename)
	name := filename[:len(filename)-len(ext)]

	return name + "." + date + ext
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
