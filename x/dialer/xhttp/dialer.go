// Package xhttp implements the client side of the xhttp (splithttp) transport
// for GOST on top of the xhttp library. The library's dialer establishes the
// session (downlink GET plus packet/stream uploads) over connections it dials
// itself, so the GOST dialer only supplies a raw dial function and caches one
// library dialer per target address to keep its connection reuse across
// sessions.
package xhttp

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"time"

	"github.com/go-gost/core/dialer"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	xctx "github.com/go-gost/x/ctx"
	xnet "github.com/go-gost/x/internal/net"
	net_dialer "github.com/go-gost/x/internal/net/dialer"
	"github.com/go-gost/x/internal/net/proxyproto"
	"github.com/go-gost/x/registry"
	xhdialer "xhttp/dialer"
)

func init() {
	registry.DialerRegistry().Register("xhttp", NewDialer)
	registry.DialerRegistry().Register("xhttps", NewTLSDialer)

	// Route the xhttp library's own diagnostics into the GOST logger.
	xhdialer.SetLogger(func(format string, v ...any) {
		logger.Default().Debugf(format, v...)
	})
}

type xhttpDialer struct {
	dialers    map[string]*xhdialer.Dialer
	mu         sync.Mutex
	tlsEnabled bool
	md         metadata
	options    dialer.Options
}

// NewDialer creates a plaintext xhttp dialer (registered as "xhttp").
func NewDialer(opts ...dialer.Option) dialer.Dialer {
	options := dialer.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &xhttpDialer{
		dialers: make(map[string]*xhdialer.Dialer),
		options: options,
	}
}

// NewTLSDialer creates a TLS xhttp dialer (registered as "xhttps").
func NewTLSDialer(opts ...dialer.Option) dialer.Dialer {
	options := dialer.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &xhttpDialer{
		dialers:    make(map[string]*xhdialer.Dialer),
		tlsEnabled: true,
		options:    options,
	}
}

func (d *xhttpDialer) Init(md md.Metadata) (err error) {
	return d.parseMetadata(md)
}

func (d *xhttpDialer) Dial(ctx context.Context, addr string, opts ...dialer.DialOption) (net.Conn, error) {
	var options dialer.DialOptions
	for _, opt := range opts {
		opt(&options)
	}

	netd := options.Dialer
	if netd == nil {
		netd = net_dialer.DefaultNetDialer
	}

	host := d.md.host
	if host == "" {
		host = options.Host
	}

	var tlsConfig *tls.Config
	if d.tlsEnabled {
		if d.options.TLSConfig != nil {
			tlsConfig = d.options.TLSConfig.Clone()
		} else {
			tlsConfig = &tls.Config{}
		}
		// The library picks the HTTP version from NextProtos: an empty list
		// means HTTP/2, a single "http/1.1" means HTTP/1.1. Offer both by
		// default so the server can choose.
		if len(tlsConfig.NextProtos) == 0 {
			tlsConfig.NextProtos = []string{"h2", "http/1.1"}
		}
	}

	d.mu.Lock()
	hd := d.dialers[addr]
	if hd == nil {
		hd = &xhdialer.Dialer{
			Address:   addr,
			Config:    d.md.config,
			TLSConfig: tlsConfig,
			Host:      host,
			RawDialContext: func(c context.Context, network, address string) (net.Conn, error) {
				// The library builds its request contexts with
				// context.WithoutCancel, which drops the dial deadline that
				// GOST's router attaches. The remaining timeout travels in a
				// context value instead (values survive WithoutCancel) so that
				// a connect that hangs is still bounded.
				if timeout, ok := c.Value(dialTimeoutKey{}).(time.Duration); ok && timeout > 0 {
					var cancel context.CancelFunc
					c, cancel = context.WithTimeout(c, timeout)
					defer cancel()
				}

				conn, err := netd.Dial(c, network, address)
				if err != nil {
					return nil, err
				}

				xnet.ApplyNoDelay(conn)

				if d.md.tcpKeepalive {
					xnet.ApplyKeepalive(conn, net.KeepAliveConfig{
						Enable:   true,
						Idle:     d.md.tcpKeepaliveIdle,
						Interval: d.md.tcpKeepaliveInterval,
						Count:    d.md.tcpKeepaliveCount,
					})
				}

				return proxyproto.WrapClientConn(
					d.options.ProxyProtocol,
					xctx.SrcAddrFromContext(c),
					xctx.DstAddrFromContext(c),
					conn,
				), nil
			},
		}
		d.dialers[addr] = hd
	}
	d.mu.Unlock()

	if timeout := dialTimeout(ctx); timeout > 0 {
		ctx = context.WithValue(ctx, dialTimeoutKey{}, timeout)
	}

	cc, err := hd.DialContext(ctx)
	if err != nil {
		if d.options.Logger != nil {
			d.options.Logger.Error(err)
		}
		return nil, err
	}

	return &conn{Conn: cc}, nil
}

// dialTimeoutKey carries the connect timeout from the GOST dial context into
// RawDialContext, which the xhttp library invokes with a context whose deadline
// has been stripped by context.WithoutCancel.
type dialTimeoutKey struct{}

// dialTimeout returns the remaining time until the context deadline, or zero
// when the context has none.
func dialTimeout(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d > 0 {
			return d
		}
	}
	return 0
}
