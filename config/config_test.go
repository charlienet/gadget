package config_test

import (
	"testing"

	"github.com/charlienet/gadget/config"
	"github.com/charlienet/gadget/config/source/env"
	"github.com/charlienet/gadget/config/source/file"
)

func TestReadFile(t *testing.T) {
	conf := config.New()

	conf.AddSource(file.WithPath("ac.toml", "toml"))
	conf.AddSource(env.New())

	_ = conf.Get("ac").String()

	app := struct{}{}
	conf.Get("app").Unmarshal(&app)
	conf.Get("app").Unmarshal(&app)
}
