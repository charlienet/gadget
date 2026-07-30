module git.charlienet.top/go/gadget/plugins/broker/kafka

go 1.26

require (
	git.charlienet.top/go/gadget/broker v0.1.5
	github.com/segmentio/kafka-go v0.4.47
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.17.2 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
	golang.org/x/net v0.25.0 // indirect
	golang.org/x/text v0.18.0 // indirect
)

replace git.charlienet.top/go/gadget/broker => ../../../broker
