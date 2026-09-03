module github.com/charlienet/gadget/plugins/ratelimit/redis

go 1.26

require (
	github.com/redis/go-redis/v9 v9.22.0
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// github.com/charlienet/gadget/ratelimit 目前是本仓库 go.work 的 use 成员，
// workspace 自动解析跨成员 import，无需（也不能）require：ratelimit/v0.1.0
// tag 尚未推送，go.mod 中写死该版本会在模块图加载时触发网络解析失败。
// 发布顺序（设计稿 §8）：推送 core tag → 本模块 go mod tidy 补 require v0.1.0
// 与 go.sum → GOWORK=off 验证 → 推送插件 tag。
