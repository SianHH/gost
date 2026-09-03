module github.com/quic-go/webtransport-go

go 1.26.0

replace github.com/apernet/quic-go v0.61.1-0.20260806010916-184d081eef3e => ../quic-go

require (
	github.com/apernet/quic-go v0.61.1-0.20260806010916-184d081eef3e
	github.com/dunglas/httpsfv v1.1.0
	github.com/stretchr/testify v1.12.1
	golang.org/x/sync v0.22.0
)

require (
	github.com/andybalholm/brotli v1.2.3 // indirect
	github.com/klauspost/compress v1.20.0 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/refraction-networking/utls v1.8.2 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)
