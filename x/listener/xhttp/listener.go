// Package xhttp implements the XHTTP (splithttp) listener for gost services.
// Register names: "xhttp" (plaintext HTTP/1.1 + h2c) and "xhttps" (TLS;
// HTTP/1.1 + HTTP/2 via ALPN).
package xhttp

import (
	"context"
	"crypto/tls"
	stdlog "log"
	"net"
	"strings"
	"time"

	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	admission "github.com/go-gost/x/admission/wrapper"
	xnet "github.com/go-gost/x/internal/net"
	"github.com/go-gost/x/internal/net/proxyproto"
	xhttp_util "github.com/go-gost/x/internal/util/xhttp"
	climiter "github.com/go-gost/x/limiter/conn/wrapper"
	limiter_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	metrics "github.com/go-gost/x/metrics/wrapper"
	stats "github.com/go-gost/x/observer/stats/wrapper"
	"github.com/go-gost/x/registry"
)

func init() {
	registry.ListenerRegistry().Register("xhttp", NewListener)
	registry.ListenerRegistry().Register("xhttps", NewTLSListener)
}

type xhttpListener struct {
	addr       net.Addr
	tlsEnabled bool
	server     *xhttp_util.Server
	errChan    chan error
	log        logger.Logger
	md         metadata
	options    listener.Options
}

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
	if l.md.keepalive {
		ln = xnet.WrapKeepaliveListener(ln, net.KeepAliveConfig{
			Enable:   true,
			Idle:     l.md.keepaliveIdle,
			Interval: l.md.keepaliveInterval,
			Count:    l.md.keepaliveCount,
		})
		l.log.Debugf("tcp keepalive enabled: idle=%v interval=%v count=%d",
			l.md.keepaliveIdle, l.md.keepaliveInterval, l.md.keepaliveCount)
	}
	ln = xnet.WrapNoDelayListener(ln)
	ln = proxyproto.WrapListener(l.options.ProxyProtocol, ln, 10*time.Second)
	ln = metrics.WrapListener(l.options.Service, ln)
	ln = stats.WrapListener(ln, l.options.Stats)
	ln = admission.WrapListener(l.options.Service, l.options.Admission, ln)
	ln = limiter_wrapper.WrapListener(l.options.Service, ln, l.options.TrafficLimiter)
	ln = climiter.WrapListener(l.options.ConnLimiter, ln)

	if l.tlsEnabled {
		tlsConfig := l.options.TLSConfig
		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		}
		tlsConfig = tlsConfig.Clone()
		// Advertise both h2 and http/1.1 so the stdlib HTTP server can
		// negotiate HTTP/2 (matching xray's splithttp hub behaviour). The
		// stdlib crypto/tls listener is required here: http.Server only
		// recognizes connections of concrete type *tls.Conn for ALPN-based
		// HTTP/2 dispatch (the gost xtls wrapper type would hide it).
		tlsConfig.NextProtos = ensureProtocols(tlsConfig.NextProtos, "h2", "http/1.1")
		ln = tls.NewListener(ln, tlsConfig)
	}

	l.addr = ln.Addr()

	l.server = xhttp_util.NewServer(
		ln,
		l.tlsEnabled,
		&l.md.config,
		xhttp_util.BacklogServerOption(l.md.backlog),
		xhttp_util.LoggerServerOption(l.log),
	)

	// Route HTTP server errors to the GOST logger instead of stderr.
	l.server.SetErrorLog(stdlog.New(&logWriter{log: l.log}, "", 0))

	l.errChan = make(chan error, 1)
	go func() {
		if err := l.server.Serve(); err != nil {
			l.errChan <- err
		}
		close(l.errChan)
	}()

	return
}

func (l *xhttpListener) Accept() (conn net.Conn, err error) {
	conn, err = l.server.Accept()
	if err != nil {
		return
	}

	// Connection limiting happens at listener level (climiter.WrapListener in
	// Init); here we apply per-connection metrics/stats/admission/traffic
	// wrappers, mirroring the pht listener.
	conn = metrics.WrapConn(l.options.Service, conn)
	conn = stats.WrapConn(conn, l.options.Stats)
	conn = admission.WrapConn(l.options.Admission, conn)
	conn = limiter_wrapper.WrapConn(
		conn,
		l.options.TrafficLimiter,
		conn.RemoteAddr().String(),
		limiter.ScopeOption(limiter.ScopeConn),
		limiter.ServiceOption(l.options.Service),
		limiter.NetworkOption(conn.LocalAddr().Network()),
		limiter.SrcOption(conn.RemoteAddr().String()),
	)
	return
}

func (l *xhttpListener) Addr() net.Addr {
	return l.addr
}

func (l *xhttpListener) Close() (err error) {
	select {
	case err = <-l.errChan:
	default:
		err = l.server.Close()
	}
	return
}

// ensureProtocols appends the given protocols to protos when they are missing.
func ensureProtocols(protos []string, defaults ...string) []string {
	out := make([]string, 0, len(protos)+len(defaults))
	out = append(out, protos...)
	for _, d := range defaults {
		found := false
		for _, p := range out {
			if p == d {
				found = true
				break
			}
		}
		if !found {
			out = append(out, d)
		}
	}
	return out
}

// logWriter adapts a GOST logger to io.Writer for http.Server.ErrorLog.
type logWriter struct {
	log logger.Logger
}

func (w *logWriter) Write(p []byte) (n int, err error) {
	w.log.Debug(strings.TrimSpace(string(p)))
	return len(p), nil
}
