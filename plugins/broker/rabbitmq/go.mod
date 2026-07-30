module git.charlienet.top/go/gadget/plugins/broker/rabbitmq

go 1.26

require (
	git.charlienet.top/go/gadget/broker v0.1.1
	github.com/rabbitmq/amqp091-go v1.10.0
)

require github.com/google/uuid v1.6.0 // indirect

replace git.charlienet.top/go/gadget/broker => ../../../broker
