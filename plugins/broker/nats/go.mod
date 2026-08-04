module github.com/charlienet/gadget/plugins/broker/nats

go 1.26

require (
	github.com/charlienet/gadget/broker v0.1.5
	github.com/nats-io/nats.go v1.37.0
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.17.2 // indirect
	github.com/nats-io/nkeys v0.4.7 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	golang.org/x/crypto v0.27.0 // indirect
	golang.org/x/sys v0.25.0 // indirect
)

replace github.com/charlienet/gadget/broker => ../../../broker
