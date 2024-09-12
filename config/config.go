package config

type ISource interface {
}

type Config interface {
}

type config struct {
}

func New(opts ...option) *config {
	return &config{}
}

func (c *config) AddSource(source ISource) {
}

func (c *config) Get(name ...string) value {
	return value{}
}
