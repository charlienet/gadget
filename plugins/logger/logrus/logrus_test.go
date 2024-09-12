package logrus_test

import (
	"fmt"
	"testing"

	"github.com/charlienet/gadget/logger"
	"github.com/charlienet/gadget/plugins/logger/logrus"
	log "github.com/sirupsen/logrus"
)

func TestLogger(t *testing.T) {
	write := func(l logger.Logger) {
		l.Debug("abc", fmt.Errorf("db error"))
		l.Info("abc", fmt.Errorf("db error"))
		l.Warn("abc", fmt.Errorf("db error"))
		l.Error("abc", fmt.Errorf("db error"))
	}

	write(logrus.DefaultLogger)

	write(logger.New(logrus.New(
		logrus.WithFormatter(&log.TextFormatter{
			ForceColors:     true,
			FullTimestamp:   true,
			TimestampFormat: "2006-01-02 15:04:05.000",
		}),
	), logger.WithLevel(logger.Debug)))

	write(logger.New(logrus.New(
		logrus.WithNestedFormatter(),
	)))
}
