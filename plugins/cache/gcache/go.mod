module git.charlienet.top/go/gadget/plugins/cache/gcache

go 1.26

require (
	git.charlienet.top/go/gadget/cache v0.1.1
	github.com/bluele/gcache v0.0.2
)

require (
	git.charlienet.top/go/gadget/logger v0.1.1 // indirect
	github.com/charlienet/go-misc v0.0.0-20240926090254-ef4f304f3a2c // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	golang.org/x/sync v0.8.0 // indirect
)

replace (
	git.charlienet.top/go/gadget/cache => ../../../cache
	git.charlienet.top/go/gadget/logger => ../../../logger
)
