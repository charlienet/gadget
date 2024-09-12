package nacos

import (
	"net"
	"strconv"

	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

type nacos struct {
	confClient config_client.IConfigClient
	group      string
}

func New(opts ...Option) nacos {
	o := Options{
		address: []string{"127.0.0.1:8848"},
	}

	for _, opt := range opts {
		opt(&o)
	}

	serverConfigs := make([]constant.ServerConfig, 0)
	for _, addr := range o.address {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
		}

		p, err := strconv.ParseUint(port, 10, 64)
		_ = err

		serverConfigs = append(serverConfigs, constant.ServerConfig{
			IpAddr: host,
			Port:   p,
		})
	}

	ic, err := clients.NewConfigClient(vo.NacosClientParam{
		ServerConfigs: serverConfigs,
		ClientConfig: &constant.ClientConfig{
			NamespaceId: o.namespace,
		},
	})

	_ = err

	return nacos{confClient: ic}
}

func (nacos) String() string { return "nacos" }
