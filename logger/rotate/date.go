package rotate

import "io"

// ensure we always implement io.WriteCloser
var _ io.WriteCloser = (*rotateDateWriter)(nil)

type rotateDateWriter struct {
	MaxAge     int
	MaxBackups int
}

func (l *rotateDateWriter) Write(p []byte) (n int, err error) {
	return 0, nil
}

func (l *rotateDateWriter) Close() error {
	return nil
}
