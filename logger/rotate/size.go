package rotate

import "io"

// ensure we always implement io.WriteCloser
var _ io.WriteCloser = (*rotateDateWriter)(nil)

type rotateSizeWriter struct {
	MaxAge     int
	MaxBackups int
}

func (l *rotateSizeWriter) Write(p []byte) (n int, err error) {
	return 0, nil
}

func (l *rotateSizeWriter) Close() error {
	return nil
}
