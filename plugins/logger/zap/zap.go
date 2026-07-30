package zap

import (
	"git.charlienet.top/go/gadget/logger"
	"go.uber.org/zap"
)

type zap_logger struct {
	l *zap.Logger
}

func New() *zap_logger {
	logger, _ := zap.NewProduction()
	return &zap_logger{l: logger}
}

func (l *zap_logger) Init(opt logger.Options) {
}
