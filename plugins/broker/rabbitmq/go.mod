module github.com/charlienet/gadget/plugins/broker/rabbitmq

go 1.26

require (
	github.com/charlienet/gadget/broker v0.1.5
	github.com/rabbitmq/amqp091-go v1.10.0
)

require github.com/google/uuid v1.6.0 // indirect

replace github.com/charlienet/gadget/broker => ../../../broker
