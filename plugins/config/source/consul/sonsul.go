package consul

import (
	capi "github.com/hashicorp/consul/api"
)

type consul struct {
	client *capi.Client
}

func New() *consul {
	config := capi.DefaultConfig()
	client, _ := capi.NewClient(config)

	return &consul{
		client: client,
	}
}

func (consul) String() string { return "consul" }
