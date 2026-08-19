package logger_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charlienet/gadget/logger"
)

// 采样：8 条同消息，first=2 保留前 2 条，之后每 3 条保留 1 条
// （第 5、8 条保留）→ 共保留 4 条
func TestSampling(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithSampling(2, 3))

	for range 8 {
		l.Info("sample msg")
	}

	if got := strings.Count(buf.String(), "sample msg"); got != 4 {
		t.Errorf("expected 4 sampled lines, got %d, output: %s", got, buf.String())
	}
}
