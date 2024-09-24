package logger_test

import (
	"os"
	"testing"

	"github.com/charlienet/gadget/logger"
)

func TestLogger(t *testing.T) {
	l := logger.DefaultLogger
	l.Debug("debug_msg1")
	l.Info("trace_msg1")
	l.Info("trace_msg1")

	l.SetOutput(os.Stdout)
	l.SetLevel(logger.Debug)

	l2 := l.WithField("req", "adsfsdfsd").WithField("bbb", "cde")
	l2.Debug("abc")
	l2.Debugf("abc%s", "测试")
}
