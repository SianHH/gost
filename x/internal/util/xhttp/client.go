package xhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/go-gost/core/logger"
	"golang.org/x/net/http2"
)

// DefaultIdleConnTimeout mirrors xray's net.ConnIdleTimeout.
const DefaultIdleConnTimeout = 10 * time.Minute

// decideHTTPVersion mirrors xray splithttp decideHTTPVersion (without the
// REALITY and HTTP/3 branches):
//   - no TLS            -> HTTP/1.1
//   - TLS with no ALPN or any ALPN list other than a single "http/1.1" -> HTTP/2
//   - TLS ALPN exactly ["http/1.1"] -> HTTP/1.1
func decideHTTPVersion(tlsConfig *tls.Config) string {
	if tlsConfig == nil {
		return "1.1"
	}
	if len(tlsConfig.NextProtos) != 1 {
		return "2"
	}
	if tlsConfig.NextProtos[0] == "http/1.1" {
		return "1.1"
	}
	return "2"
}

// connSource hands out the (single) base connection first, then dials extra
// connections on demand. The base connection is the one gost dialed in the
// chain's Dial step; extra connections are needed by HTTP/1.1 mode where the
// download response body occupies the base connection for the whole session.
type connSource struct {
	mu       sync.Mutex
	base     net.Conn
	baseUsed bool
	prep     func(conn net.Conn) (net.Conn, error)
	dial     func(ctx context.Context) (net.Conn, error)
	closed   bool
	conns    map[net.Conn]struct{}
}

func newConnSource(base net.Conn, prep func(net.Conn) (net.Conn, error), dial func(ctx context.Context) (net.Conn, error)) *connSource {
	s := &connSource{
		base:  base,
		prep:  prep,
		dial:  dial,
		conns: make(map[net.Conn]struct{}),
	}
	// Track the base connection from the very beginning so it is always
	// released on close, even if it was never handed out to a request.
	if base != nil {
		s.conns[base] = struct{}{}
	}
	return s
}

func (s *connSource) get(ctx context.Context) (net.Conn, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("xhttp: connection source is closed")
	}
	if !s.baseUsed {
		s.baseUsed = true
		conn := s.base
		s.mu.Unlock()
		return s.prepare(conn)
	}
	s.mu.Unlock()

	if s.dial == nil {
		return nil, errors.New("xhttp: no dialer for extra connections")
	}
	conn, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	return s.prepare(conn)
}

func (s *connSource) prepare(conn net.Conn) (net.Conn, error) {
	c, err := s.prep(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	return c, nil
}

func (s *connSource) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		c.Close()
	}
}

// client wraps the HTTP round tripper used by a single XHTTP session. A fresh
// client (and a fresh http.Transport bound to the specific base connection) is
// created for every gost connection, so no cross-session state is shared.
type client struct {
	config      *Config
	httpVersion string
	client      *http.Client
	rt          http.RoundTripper
	src         *connSource
	log         logger.Logger
}

func newClient(ctx context.Context, cfg *Config, base net.Conn, tlsConfig *tls.Config,
	dial func(ctx context.Context) (net.Conn, error), log logger.Logger) (*client, error) {

	httpVersion := decideHTTPVersion(tlsConfig)

	prep := func(conn net.Conn) (net.Conn, error) {
		if tlsConfig == nil {
			return conn, nil
		}
		tc := tlsConfig.Clone()
		if tc.ServerName == "" {
			if host, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
				tc.ServerName = host
			}
		}
		// Make sure the client advertises an ALPN that matches the negotiated
		// HTTP version. When the user did not configure ALPN, decideHTTPVersion
		// already picked HTTP/2 (xray default logic), so we must advertise it.
		if httpVersion == "2" {
			if !hasProtocol(tc.NextProtos, "h2") {
				tc.NextProtos = append([]string{"h2"}, tc.NextProtos...)
			}
		} else if len(tc.NextProtos) == 0 {
			tc.NextProtos = []string{"http/1.1"}
		}
		tconn := tls.Client(conn, tc)
		if err := tconn.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		if httpVersion == "2" {
			state := tconn.ConnectionState()
			if state.NegotiatedProtocol != "h2" {
				return nil, fmt.Errorf("xhttp: server negotiated %q, want h2", state.NegotiatedProtocol)
			}
		}
		return tconn, nil
	}

	src := newConnSource(base, prep, dial)

	var rt http.RoundTripper
	if httpVersion == "2" {
		rt = &http2.Transport{
			DialTLSContext: func(c context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
				return src.get(c)
			},
			IdleConnTimeout: DefaultIdleConnTimeout,
		}
	} else {
		rt = &http.Transport{
			// Keep-alives stay DISABLED for HTTP/1.1. net/http cannot
			// concurrently drive the long-lived download GET plus the
			// stream-up/packet-up uploads over pooled HTTP/1.1 connections
			// (enabling keep-alives deadlocked stream-up sessions). xray makes
			// the same choice for the transport and reuses raw sockets through
			// its own uploadRawPool/H1Conn instead — net/http's idle pool is
			// not an equivalent substitute here.
			DisableKeepAlives: true,
			IdleConnTimeout:   DefaultIdleConnTimeout,
			MaxIdleConns:      8,
			DialContext: func(c context.Context, network, addr string) (net.Conn, error) {
				return src.get(c)
			},
			DialTLSContext: func(c context.Context, network, addr string) (net.Conn, error) {
				return src.get(c)
			},
		}
	}

	return &client{
		config:      cfg,
		httpVersion: httpVersion,
		client: &http.Client{
			Transport: rt,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		rt:  rt,
		src: src,
		log: log,
	}, nil
}

func (c *client) close() {
	switch tr := c.rt.(type) {
	case *http.Transport:
		tr.CloseIdleConnections()
	case *http2.Transport:
		tr.CloseIdleConnections()
	}
	c.src.close()
}

// do performs a single request and returns the response.
func (c *client) do(ctx context.Context, method, url string, sessionID, seqStr string,
	payload []byte, stream bool, body io.Reader) (*http.Response, error) {

	var req *http.Request
	var err error
	if stream {
		req, err = http.NewRequestWithContext(ctx, method, url, body)
		if err != nil {
			return nil, err
		}
		c.config.FillStreamRequest(req, sessionID, seqStr)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return nil, err
		}
		c.config.FillPacketRequest(req, sessionID, seqStr, payload)
	}
	return c.client.Do(req)
}

// openStream sends a stream request (GET/POST) and returns a read closer that
// blocks until the response headers arrive. Like xray's OpenStream, it waits
// only until the underlying connection is established (GotConn) before
// returning, so buffering middleboxes cannot stall the handshake. HTTP-level
// failures (non-200) surface as errors on the first Read.
func (c *client) openStream(ctx context.Context, method, url string, sessionID string,
	body io.Reader) (io.ReadCloser, error) {

	gotConn := make(chan error, 1)
	ictx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			select {
			case gotConn <- nil:
			default:
			}
		},
	})

	req, err := http.NewRequestWithContext(ictx, method, url, body)
	if err != nil {
		return nil, err
	}
	c.config.FillStreamRequest(req, sessionID, "")

	wrc := newWaitReadCloser()
	go func() {
		resp, err := c.client.Do(req)
		if err != nil {
			wrc.deliver(nil, err)
			return
		}
		if resp.StatusCode != 200 {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			wrc.deliver(nil, fmt.Errorf("xhttp: unexpected status %d for %s %s", resp.StatusCode, method, url))
			return
		}
		wrc.deliver(resp.Body, nil)
	}()

	select {
	case err := <-gotConn:
		if err != nil {
			return nil, err
		}
	case <-wrc.done:
		// The request finished before a connection was ever established
		// (e.g. TLS handshake failure); surface its error immediately.
		if err := wrc.errValue(); err != nil {
			return nil, err
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return wrc, nil
}

// waitReadCloser becomes readable once the asynchronous request completes. A
// Read before the response arrives blocks; a failed request surfaces its error
// on Read.
type waitReadCloser struct {
	mu    sync.Mutex
	ready chan struct{}
	done  chan struct{}
	rc    io.ReadCloser
	err   error
}

func newWaitReadCloser() *waitReadCloser {
	return &waitReadCloser{
		ready: make(chan struct{}),
		done:  make(chan struct{}),
	}
}

func (w *waitReadCloser) deliver(rc io.ReadCloser, err error) {
	w.mu.Lock()
	select {
	case <-w.ready:
		// Already closed by Close(); drop the payload.
		w.mu.Unlock()
		if rc != nil {
			rc.Close()
		}
		return
	default:
	}
	w.rc = rc
	w.err = err
	close(w.ready)
	close(w.done)
	w.mu.Unlock()
}

func (w *waitReadCloser) errValue() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *waitReadCloser) Read(p []byte) (int, error) {
	<-w.ready
	w.mu.Lock()
	rc, err := w.rc, w.err
	w.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if rc == nil {
		return 0, io.ErrClosedPipe
	}
	return rc.Read(p)
}

func (w *waitReadCloser) Close() error {
	w.mu.Lock()
	select {
	case <-w.ready:
	default:
		w.err = io.ErrClosedPipe
		close(w.ready)
		close(w.done)
	}
	rc := w.rc
	w.mu.Unlock()
	if rc != nil {
		return rc.Close()
	}
	return nil
}

// postPacket uploads a single packet-up payload.
func (c *client) postPacket(ctx context.Context, url string, sessionID string, seqStr string, payload []byte) error {
	method := c.config.GetNormalizedUplinkHTTPMethod()
	resp, err := c.do(ctx, method, url, sessionID, seqStr, payload, false, nil)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("xhttp: bad status code: %s", resp.Status)
	}
	return nil
}

func hasProtocol(protos []string, want string) bool {
	for _, p := range protos {
		if p == want {
			return true
		}
	}
	return false
}
