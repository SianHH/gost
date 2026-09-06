// Package xhttp implements the XHTTP (splithttp) dialer for the gost chain.
// Register names: "xhttp" (plaintext HTTP/1.1) and "xhttps" (TLS; HTTP/2 by
// default, HTTP/1.1 when ALPN is explicitly http/1.1).
package xhttp

import (
	"context"
	"net"
	"sync"

	"github.com/go-gost/core/dialer"
	md "github.com/go-gost/core/metadata"
	xhttp_util "github.com/go-gost/x/internal/util/xhttp"
	"github.com/go-gost/x/registry"
)

func init() {
	registry.DialerRegistry().Register("xhttp", NewDialer)
	registry.DialerRegistry().Register("xhttps", NewTLSDialer)
}

type xhttpDialer struct {
	md         metadata
	tlsEnabled bool
	options    dialer.Options

	// conns remembers, per base connection handed out by Dial, how to dial
	// additional raw connections (needed by HTTP/1.1 mode where the download
	// GET occupies the base connection for the whole session).
	conns sync.Map
}

func NewDialer(opts ...dialer.Option) dialer.Dialer {
	options := dialer.Options{}
	for _, opt := range opts {
		opt(&options)
	}

	return &xhttpDialer{
		options: options,
	}
}

func NewTLSDialer(opts ...dialer.Option) dialer.Dialer {
	options := dialer.Options{}
	for _, opt := range opts {
		opt(&options)
	}

	return &xhttpDialer{
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

	conn, err := options.Dialer.Dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	extraDial := func(ectx context.Context) (net.Conn, error) {
		return options.Dialer.Dial(ectx, "tcp", addr)
	}
	d.conns.Store(conn, extraDial)

	return conn, nil
}

// Handshake implements dialer.Handshaker. It turns the raw base connection
// into an XHTTP session (stream-down GET + stream-up POST / packet-up POSTs)
// and returns the split connection the proxy payload travels over.
func (d *xhttpDialer) Handshake(ctx context.Context, conn net.Conn, options ...dialer.HandshakeOption) (net.Conn, error) {
	opts := &dialer.HandshakeOptions{}
	for _, option := range options {
		option(opts)
	}

	var extraDial func(context.Context) (net.Conn, error)
	if v, ok := d.conns.LoadAndDelete(conn); ok {
		extraDial = v.(func(context.Context) (net.Conn, error))
	}

	var tlsConfig = d.options.TLSConfig
	if !d.tlsEnabled {
		tlsConfig = nil // plaintext: xhttp not xhttps
	}

	urlHost := d.md.config.Host
	if urlHost == "" {
		if tlsConfig != nil && tlsConfig.ServerName != "" {
			urlHost = tlsConfig.ServerName
		} else if host, _, err := net.SplitHostPort(opts.Addr); err == nil {
			urlHost = host
		} else {
			urlHost = opts.Addr
		}
	}

	return xhttp_util.Dial(ctx, xhttp_util.DialOptions{
		Config:    &d.md.config,
		BaseConn:  conn,
		ExtraDial: extraDial,
		TLSConfig: tlsConfig,
		URLHost:   urlHost,
		Logger:    d.options.Logger,
	})
}
