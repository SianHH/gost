// Package xhttp implements the xhttp (splithttp) transport for GOST on top of
// the xhttp library. The library exposes the protocol as a standard
// net.Listener, so this package only adapts it to the GOST listener interface:
// it builds the base TCP listener, applies the standard GOST listener
// wrappers (proxy protocol, metrics, stats, admission, limiters), and
// optionally terminates TLS before handing the socket to the xhttp server.
package xhttp

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	admission "github.com/go-gost/x/admission/wrapper"
	xnet "github.com/go-gost/x/internal/net"
	"github.com/go-gost/x/internal/net/proxyproto"
	climiter "github.com/go-gost/x/limiter/conn/wrapper"
	limiter_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	metrics "github.com/go-gost/x/metrics/wrapper"
	stats "github.com/go-gost/x/observer/stats/wrapper"
	"github.com/go-gost/x/registry"
	xhlistener "xhttp/listener"
)

func init() {
	registry.ListenerRegistry().Register("xhttp", NewListener)
	registry.ListenerRegistry().Register("xhttps", NewTLSListener)

	// Route the xhttp library's own diagnostics into the GOST logger.
	xhlistener.SetLogger(func(format string, v ...any) {
		logger.Default().Debugf(format, v...)
	})
}

type xhttpListener struct {
	addr       net.Addr
	ln         net.Listener
	tlsEnabled bool
	log        logger.Logger
	md         metadata
	options    listener.Options
}

// NewListener creates a plaintext xhttp listener (registered as "xhttp").
func NewListener(opts ...listener.Option) listener.Listener {
	options := listener.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &xhttpListener{
		log:     options.Logger,
		options: options,
	}
}

// NewTLSListener creates a TLS xhttp listener (registered as "xhttps").
func NewTLSListener(opts ...listener.Option) listener.Listener {
	options := listener.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &xhttpListener{
		tlsEnabled: true,
		log:        options.Logger,
		options:    options,
	}
}

func (l *xhttpListener) Init(md md.Metadata) (err error) {
	if err = l.parseMetadata(md); err != nil {
		return
	}

	network := "tcp"
	if xnet.IsIPv4(l.options.Addr) {
		network = "tcp4"
	}

	lc := net.ListenConfig{}
	if l.md.mptcp {
		lc.SetMultipathTCP(true)
		l.log.Debugf("mptcp enabled: %v", lc.MultipathTCP())
	}
	ln, err := lc.Listen(context.Background(), network, l.options.Addr)
	if err != nil {
		return
	}
	if l.md.tcpKeepalive {
		ln = xnet.WrapKeepaliveListener(ln, net.KeepAliveConfig{
			Enable:   true,
			Idle:     l.md.tcpKeepaliveIdle,
			Interval: l.md.tcpKeepaliveInterval,
			Count:    l.md.tcpKeepaliveCount,
		})
		l.log.Debugf("tcp keepalive enabled: idle=%v interval=%v count=%d",
			l.md.tcpKeepaliveIdle, l.md.tcpKeepaliveInterval, l.md.tcpKeepaliveCount)
	}
	ln = xnet.WrapNoDelayListener(ln)
	ln = proxyproto.WrapListener(l.options.ProxyProtocol, ln, 10*time.Second)
	ln = metrics.WrapListener(l.options.Service, ln)
	ln = stats.WrapListener(ln, l.options.Stats)
	ln = admission.WrapListener(l.options.Service, l.options.Admission, ln)
	ln = limiter_wrapper.WrapListener(l.options.Service, ln, l.options.TrafficLimiter)
	ln = climiter.WrapListener(l.options.ConnLimiter, ln)

	l.addr = ln.Addr()

	// The xhttp server performs its own TLS handshake (so that ALPN can be
	// negotiated and HTTP/2 over TLS is enabled), which is why the TLS config
	// is handed to the library rather than wrapping the listener here.
	var tlsConfig *tls.Config
	if l.tlsEnabled {
		if l.options.TLSConfig != nil {
			tlsConfig = l.options.TLSConfig.Clone()
		} else {
			tlsConfig = &tls.Config{}
		}
		if len(tlsConfig.NextProtos) == 0 {
			tlsConfig.NextProtos = []string{"h2", "http/1.1"}
		}
	}

	xln, err := xhlistener.ListenWithListener(context.Background(), ln, l.md.config, tlsConfig)
	if err != nil {
		ln.Close()
		return
	}
	l.ln = xln

	return
}

func (l *xhttpListener) Accept() (conn net.Conn, err error) {
	conn, err = l.ln.Accept()
	if err != nil {
		return nil, err
	}

	return limiter_wrapper.WrapConn(
		conn,
		l.options.TrafficLimiter,
		conn.RemoteAddr().String(),
		limiter.ScopeOption(limiter.ScopeConn),
		limiter.ServiceOption(l.options.Service),
		limiter.NetworkOption(conn.LocalAddr().Network()),
		limiter.SrcOption(conn.RemoteAddr().String()),
	), nil
}

func (l *xhttpListener) Close() error {
	if l.ln == nil {
		return nil
	}
	return l.ln.Close()
}

func (l *xhttpListener) Addr() net.Addr {
	return l.addr
}
