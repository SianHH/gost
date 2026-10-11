# xhttp

A dependency-free port of the Xray-core **XHTTP** transport (formerly
`splithttp`), exposed as a standard `net.Listener` and a standard `net.Conn`
dialer so it can be dropped into any Go project that speaks `net.Conn`.

The module has **no third-party dependencies** — only the Go standard library.

## Usage

### Server

```go
import (
	"context"
	"crypto/tls"
	"io"
	"net"

	"xhttp/config"
	"xhttp/listener"
)

serverTLS := &tls.Config{ /* certificates; add "h2" to NextProtos for HTTP/2 */ }

ln, err := listener.Listen(context.Background(), "0.0.0.0:443", &config.Config{
	Path: "/your/path",
	Mode: "auto",
}, serverTLS)
if err != nil {
	return err
}
defer ln.Close()

for {
	conn, err := ln.Accept()
	if err != nil {
		return err
	}
	go func(c net.Conn) {
		defer c.Close()
		io.Copy(c, c)
	}(conn)
}
```

Pass a `nil` `*tls.Config` to serve plain HTTP/1.1 and h2c.

`listener.ListenWithListener` serves XHTTP on an already-created `net.Listener`
(useful for socket activation, unix sockets, or wrapping an existing socket).

### Client

```go
import (
	"context"
	"crypto/tls"

	"xhttp/config"
	"xhttp/dialer"
)

d := &dialer.Dialer{
	Address:   "example.com:443",
	Config:    &config.Config{Path: "/your/path"},
	TLSConfig: &tls.Config{ServerName: "example.com"},
}

conn, err := d.DialContext(ctx)
if err != nil {
	return err
}
defer conn.Close()

// conn is a net.Conn: write to upload, read to download.
```

`dialer.Dial` is a convenience wrapper that creates a throwaway `Dialer` for a
single session. **Reuse one `Dialer` per target**: it owns the HTTP transports
and the connection-reuse (xmux) state, so a fresh `Dialer` per session defeats
multiplexing and connection reuse.

The `Dialer` fields:

| Field | Meaning |
| --- | --- |
| `Address` | `host:port` to dial. Required. |
| `Config` | XHTTP protocol configuration. Required. |
| `TLSConfig` | Non-nil switches to HTTPS. `NextProtos` selects the HTTP version (see below). |
| `Host` | Overrides the HTTP `Host` header; defaults to `Config.Host`, then `TLSConfig.ServerName`, then the host part of `Address`. |
| `RawDialContext` | Optional override for establishing the underlying TCP connection. |
| `Timeout` | Dial timeout used when `RawDialContext` is nil. |

## HTTP version selection

For the client, the HTTP version is chosen from `TLSConfig.NextProtos`:

- no `TLSConfig` → HTTP/1.1 over plain TCP (h2c is used if the server upgrades)
- `NextProtos == ["http/1.1"]` → HTTP/1.1
- anything else → HTTP/2 via ALPN (`"h2"`)

For the server, the listener always accepts HTTP/1.1 and unencrypted HTTP/2;
add `"h2"` to the TLS config's `NextProtos` to also serve HTTP/2 over TLS.

## Configuration

`config.Config` mirrors the fields of the upstream protocol config. Notable
fields:

- `Path` — base request path; may include a query string (`/path?x=1`).
- `Host` — server-side allowed `Host` value; client-side default URL host.
- `Mode` — `""`/`"auto"` (client picks `packet-up`), `packet-up`,
  `stream-up`, or `stream-one`.
- `Headers` — extra request headers. A `User-Agent` of `chrome`, `firefox`,
  `edge` or `golang` selects that browser fingerprint; otherwise a Chrome-like
  default (with `Sec-*` client hints) is used.
- `XPaddingBytes`, `XPaddingObfsMode`, `XPaddingKey`, `XPaddingHeader`,
  `XPaddingPlacement`, `XPaddingMethod` — request/response padding, used to
  defeat traffic analysis.
- `ScMaxEachPostBytes`, `ScMinPostsIntervalMs`, `ScMaxBufferedPosts`,
  `ScStreamUpServerSecs` — upload batching and buffering limits.
- `UplinkHTTPMethod`, `SessionPlacement`/`SessionKey`,
  `SeqPlacement`/`SeqKey`, `UplinkDataPlacement`/`UplinkDataKey`,
  `UplinkChunkSize` — where session id, sequence number and uplink payload are
  carried.
- `NoGRPCHeader`, `NoSSEHeader`, `ServerMaxHeaderBytes`.
- `Xmux` — client-side transport/concurrency reuse policy. When nil or
  completely empty, the same defaults as Xray-core's config layer are applied
  (see `Config.GetNormalizedXmux`).

Nil pointer fields fall back to the upstream defaults, so most users only set
`Path`, `Mode`, `Host` and `Headers`.

## Logging

Logging is silent by default. Install a sink with:

```go
dialer.SetLogger(log.Printf)   // or listener.SetLogger
```

## Differences from Xray-core's `splithttp`

This package intentionally covers the protocol itself, not the Xray transport
plumbing. The following upstream features are **not** implemented:

- **HTTP/3 (QUIC)**. Only HTTP/1.1 and HTTP/2 are supported, because the QUIC
  transport requires `quic-go`.
- **REALITY** and **TLS fingerprint (uTLS)**. Use `RawDialContext` to plug in
  your own TLS/REALITY stack.
- **Browser dialer** (the `dialerProxy` / `browserDialer` mode).
- **Separate download settings** (`downloadSettings`), i.e. a different
  transport for the downlink than for the uplink.
- **Socket options, TCP/UDP masks, traffic statistics and Xray's connection
  handling interface**. Wrap the returned `net.Conn`/`net.Listener` instead.
- **HTTP/2 keep-alive tuning** (`hKeepAlivePeriod`). net/http's bundled HTTP/2
  client exposes no `ReadIdleTimeout`; upstream uses `golang.org/x/net/http2`.

Two upstream bugs are fixed here: the upload reassembly queue uses atomics so
it is race-detector clean, and HTTP/1.1 upload connections are counted so their
pipelined responses are drained (upstream leaves that counter at zero).

## Copying into another project

The package is a self-contained Go module. Two options:

1. **Keep it as a nested module** and point the host module at it:

   ```
   # in the host project's go.mod
   require xhttp v0.0.0
   replace xhttp => ./xhttp
   ```

2. **Vendor it into an existing module** and rewrite the import prefix from
   `xhttp/` to the host module path, e.g.:

   ```
   sed -i 's#"xhttp/#"example.com/you/project/third_party/xhttp/#g' $(find xhttp -name '*.go')
   rm xhttp/go.mod
   ```
