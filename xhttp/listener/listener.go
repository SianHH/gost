// Package listener exposes the xhttp transport as a standard net.Listener.
//
// Accepted connections correspond to xhttp sessions: a connection becomes
// available to Accept once its downlink GET arrives at the server.
package listener

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"xhttp/config"
	"xhttp/internal/logger"
)

// SetLogger installs a logging sink. Messages are discarded by default.
func SetLogger(f func(format string, v ...any)) { logger.Set(f) }

// Listener is a net.Listener that speaks xhttp.
type Listener struct {
	config    *config.Config
	server    *http.Server
	listener  net.Listener
	handler   *requestHandler
	acceptCh  chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

// Listen listens on address ("host:port") and returns an xhttp Listener.
//
// When tlsConfig is non-nil the listener speaks HTTPS; include "h2" in
// tlsConfig.NextProtos to also accept HTTP/2.
func Listen(ctx context.Context, address string, cfg *config.Config, tlsConfig *tls.Config) (*Listener, error) {
	base, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	l, err := ListenWithListener(ctx, base, cfg, tlsConfig)
	if err != nil {
		base.Close()
		return nil, err
	}
	return l, nil
}

// ListenWithListener serves xhttp on an already-created net.Listener. This is
// useful for reusing an existing socket or for wrapping a custom listener.
func ListenWithListener(ctx context.Context, base net.Listener, cfg *config.Config, tlsConfig *tls.Config) (*Listener, error) {
	if cfg == nil {
		return nil, fmt.Errorf("xhttp/listener: Config is nil")
	}
	if base == nil {
		return nil, fmt.Errorf("xhttp/listener: listener is nil")
	}

	l := &Listener{
		config:   cfg,
		acceptCh: make(chan net.Conn),
		done:     make(chan struct{}),
	}

	ln := base
	if tlsConfig != nil {
		ln = tls.NewListener(ln, tlsConfig)
	}
	l.listener = ln

	l.handler = &requestHandler{
		config:    cfg,
		host:      cfg.Host,
		path:      cfg.GetNormalizedPath(),
		ln:        l,
		sessionMu: &sync.Mutex{},
		localAddr: ln.Addr(),
	}

	// The server handles plaintext HTTP/1.1 and h2c (and, over TLS, HTTP/2).
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	l.server = &http.Server{
		Handler:           l.handler,
		ReadHeaderTimeout: 4 * time.Second,
		MaxHeaderBytes:    cfg.GetNormalizedServerMaxHeaderBytes(),
		Protocols:         protocols,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	go func() {
		if err := l.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("xhttp: server stopped: %v", err)
		}
	}()

	logger.Printf("xhttp: listening on %s", ln.Addr())
	return l, nil
}

// Accept waits for and returns the next xhttp session.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.acceptCh:
		if conn == nil {
			return nil, net.ErrClosed
		}
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops the listener. Existing sessions are not terminated.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
		if l.listener != nil {
			l.listener.Close()
		}
		if l.server != nil {
			l.server.Close()
		}
	})
	return nil
}

// Addr returns the listener's network address.
func (l *Listener) Addr() net.Addr {
	if l.listener == nil {
		return nil
	}
	return l.listener.Addr()
}

// addConn hands a completed session to Accept.
func (l *Listener) addConn(c net.Conn) {
	select {
	case l.acceptCh <- c:
	case <-l.done:
		c.Close()
	}
}
