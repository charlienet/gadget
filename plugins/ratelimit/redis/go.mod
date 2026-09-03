module github.com/charlienet/gadget/plugins/ratelimit/redis

go 1.26

require (
	github.com/charlienet/gadget/ratelimit v0.1.0
	github.com/redis/go-redis/v9 v9.22.0
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/charlienet/gadget/breaker v0.1.0 // indirect
	github.com/hashicorp/go-version v1.9.0 // indirect
	github.com/kr/text v0.2.0 // indirect
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charlienet/gadget/redis v0.4.2
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// 依赖说明：本模块发布遵循设计稿 §8 顺序——ratelimit/v0.1.0 tag 推送后，
// 以 GOWORK=off go mod tidy 补齐上方 require 与 go.sum，再验证并发插件 tag。
