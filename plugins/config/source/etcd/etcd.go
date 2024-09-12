package etcd

import clientv3 "go.etcd.io/etcd/client/v3"

type etcd struct {
	prefix string
	client *clientv3.Client
}

func New(opts ...Option) *etcd {
	opt := Options{
		Endpoints: []string{"localhost:2379"},
	}

	for _, o := range opts {
		o(&opt)
	}

	config := clientv3.Config{
		Endpoints:   opt.Endpoints,
		DialTimeout: opt.DialTimout,
	}

	client, err := clientv3.New(config)
	_ = err

	return &etcd{
		client: client,
	}
}

func (c *etcd) Read() {
}

func (c *etcd) Watch() *watcher {
	return newWatcher(c.prefix, c.client.Watcher)
}

func (etcd) String() string { return "etcd" }
