package logger_test

import (
	"testing"

	"github.com/charlienet/gadget/logger"
)

func TestLogger(t *testing.T) {
	l := logger.DefaultLogger
	l.Info("trace_msg1")
}
