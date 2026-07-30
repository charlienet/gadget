module git.charlienet.top/go/gadget/plugins/cache/redis

go 1.26

require (
	git.charlienet.top/go/gadget/cache v0.1.5
	git.charlienet.top/go/gadget/redis v0.1.5
	git.charlienet.top/go/gadget/test v0.1.5
	github.com/charlienet/go-misc v0.0.0-20240926090254-ef4f304f3a2c
	github.com/redis/go-redis/v9 v9.6.1
	github.com/stretchr/testify v1.9.0
)

require (
	git.charlienet.top/go/gadget/logger v0.1.5 // indirect
	github.com/alicebob/gopher-json v0.0.0-20230218143504-906a9b012302 // indirect
	github.com/alicebob/miniredis v2.5.0+incompatible // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/go-redis/redis_rate/v10 v10.0.1 // indirect
	github.com/gomodule/redigo v1.9.2 // indirect
	github.com/hashicorp/go-version v1.7.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	golang.org/x/sync v0.8.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	git.charlienet.top/go/gadget/cache => ../../../cache
	git.charlienet.top/go/gadget/redis => ../../../redis
	git.charlienet.top/go/gadget/test => ../../../test
)
