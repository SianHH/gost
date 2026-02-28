module github.com/apernet/quic-go

go 1.25.0

require (
	github.com/quic-go/qpack v0.6.0
	github.com/stretchr/testify v1.11.1
	go.uber.org/mock v0.6.0
	golang.org/x/crypto v0.48.0
	golang.org/x/net v0.51.0
	golang.org/x/sync v0.19.0
	golang.org/x/sys v0.41.0
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/jordanlewis/gcassert v0.0.0-20250430164644-389ef753e22e // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	golang.org/x/mod v0.33.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	golang.org/x/tools v0.42.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

tool (
	github.com/jordanlewis/gcassert/cmd/gcassert
	go.uber.org/mock/mockgen
)
