module github.com/charlienet/gadget/plugins/logger/zap

go 1.26

require (
	github.com/charlienet/gadget/logger v0.1.5
	go.uber.org/zap v1.28.0
)

require go.uber.org/multierr v1.10.0 // indirect

replace github.com/charlienet/gadget/logger => ../../../logger
