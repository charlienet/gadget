package config_test

import (
	"testing"

	"git.charlienet.top/go/gadget/config"
	"git.charlienet.top/go/gadget/config/source/env"
	"git.charlienet.top/go/gadget/config/source/file"
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
