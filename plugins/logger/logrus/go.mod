module git.charlienet.top/go/gadget/plugins/logger/logrus

go 1.26

require (
	git.charlienet.top/go/gadget/logger v0.1.1
	github.com/antonfisher/nested-logrus-formatter v1.3.1
	github.com/sirupsen/logrus v1.9.3
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/stretchr/testify v1.9.0 // indirect
	golang.org/x/sys v0.25.0 // indirect
)

replace git.charlienet.top/go/gadget/logger => ../../../logger
