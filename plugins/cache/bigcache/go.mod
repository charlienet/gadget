module git.charlienet.top/go/gadget/plugins/cache/bigcache

go 1.26

require (
	git.charlienet.top/go/gadget/cache v0.1.5
	github.com/allegro/bigcache/v3 v3.1.0
	github.com/stretchr/testify v1.9.0
)

require (
	git.charlienet.top/go/gadget/logger v0.1.5 // indirect
	github.com/charlienet/go-misc v0.0.0-20240926090254-ef4f304f3a2c // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	golang.org/x/sync v0.8.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	git.charlienet.top/go/gadget/cache => ../../../cache
	git.charlienet.top/go/gadget/redis => ../../../redis
)
