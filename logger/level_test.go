package logger_test

import (
	"testing"

	"github.com/charlienet/gadget/logger"
)

func TestLevelEnable(t *testing.T) {
	t.Log(logger.Debug.Enabled(logger.Error))
}
