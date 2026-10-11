// Package dialer exposes the xhttp transport as a client that dials a target
// address and returns a standard net.Conn.
//
// A Dialer is bound to a single target address and reuses its underlying HTTP
// transports across calls, so create one Dialer per target and reuse it for
// every session you open.
package dialer

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"xhttp/config"
	"xhttp/internal/logger"
	"xhttp/internal/pipebuffer"
	"xhttp/internal/splitconn"
)

const (
	connIdleTimeout = 300 * time.Second
)

// SetLogger installs a logging sink. Messages are discarded by default.
func SetLogger(f func(format string, v ...any)) { logger.Set(f) }

// Dialer establishes xhttp sessions to a single target Address.
type Dialer struct {
	// Address is the "host:port" to connect to. Required.
	Address string
	// Config is the xhttp protocol configuration. Required.
	Config *config.Config
	// TLSConfig, when non-nil, switches the transport to HTTPS. Its NextProtos
	// selects the HTTP version: exactly ["http/1.1"] means HTTP/1.1, anything
	// else (including empty) means HTTP/2.
	TLSConfig *tls.Config
	// Host overrides the HTTP Host header / URL host. It defaults to
	// Config.Host, then TLSConfig.ServerName, then the host part of Address.
	Host string
	// RawDialContext optionally overrides how the underlying TCP connection is
	// established. When nil, a net.Dialer with Timeout is used.
	RawDialContext func(ctx context.Context, network, address string) (net.Conn, error)
	// Timeout bounds connection establishment when RawDialContext is nil.
	Timeout time.Duration

	mu          sync.Mutex
	initialized bool
	requestURL  string
	httpVersion string
	xmuxManager *XmuxManager
}

// Dial opens a single xhttp session using a throwaway Dialer. Reuse a Dialer
// instead when you open more than one session to the same target.
func Dial(ctx context.Context, address string, cfg *config.Config, tlsConfig *tls.Config) (net.Conn, error) {
	d := &Dialer{Address: address, Config: cfg, TLSConfig: tlsConfig}
	return d.DialContext(ctx)
}

// DialContext opens an xhttp session and returns it as a net.Conn.
func (d *Dialer) DialContext(ctx context.Context) (net.Conn, error) {
	if d.Config == nil {
		return nil, fmt.Errorf("xhttp/dialer: Config is nil")
	}
	if d.Address == "" {
		return nil, fmt.Errorf("xhttp/dialer: Address is empty")
	}
	d.init()

	cfg := d.Config
	mode := cfg.Mode
	if mode == "" || mode == "auto" {
		mode = "packet-up"
	}

	sessionId := ""
	if mode != "stream-one" {
		sessionId = newSessionID()
	}

	httpClient, xmuxClient := d.getHTTPClient(ctx)
	if xmuxClient != nil {
		xmuxClient.OpenUsage.Add(1)
	}

	var closed atomic.Int32
	release := func() {
		if closed.Add(1) > 1 {
			return
		}
		if xmuxClient != nil {
			xmuxClient.OpenUsage.Add(-1)
		}
	}

	conn := splitconn.New()
	conn.SetOnClose(release)

	logger.Printf("xhttp: dialing %s, mode %s, HTTP/%s, host %s", d.Address, mode, d.httpVersion, d.urlHost())

	if mode == "stream-one" {
		bodyReader, bodyWriter := io.Pipe()
		conn.SetWriter(bodyWriter)
		if xmuxClient != nil {
			xmuxClient.LeftRequests.Add(-1)
		}
		rc, remoteAddr, localAddr, err := httpClient.OpenStream(ctx, d.requestURL, sessionId, bodyReader, false)
		if err != nil {
			return nil, err
		}
		conn.SetReader(rc)
		conn.SetRemoteAddr(remoteAddr)
		conn.SetLocalAddr(localAddr)
		return conn, nil
	}

	// Downlink: a long-lived GET that every non stream-one mode needs.
	if xmuxClient != nil {
		xmuxClient.LeftRequests.Add(-1)
	}
	rc, remoteAddr, localAddr, err := httpClient.OpenStream(ctx, d.requestURL, sessionId, nil, false)
	if err != nil {
		return nil, err
	}
	conn.SetReader(rc)
	conn.SetRemoteAddr(remoteAddr)
	conn.SetLocalAddr(localAddr)

	if mode == "stream-up" {
		bodyReader, bodyWriter := io.Pipe()
		conn.SetWriter(bodyWriter)
		if xmuxClient != nil {
			xmuxClient.LeftRequests.Add(-1)
		}
		if _, _, _, err := httpClient.OpenStream(ctx, d.requestURL, sessionId, bodyReader, true); err != nil {
			return nil, err
		}
		return conn, nil
	}

	// packet-up: batch application writes and ship them as separate POSTs.
	scMaxEachPostBytes := cfg.GetNormalizedScMaxEachPostBytes()
	scMinPostsIntervalMs := cfg.GetNormalizedScMinPostsIntervalMs()
	if scMaxEachPostBytes.From <= 0 {
		return nil, fmt.Errorf("xhttp: ScMaxEachPostBytes must be greater than 0")
	}

	maxUploadSize := scMaxEachPostBytes.Rand()
	uploadBuf := pipebuffer.New(max(1, int(maxUploadSize)-2048))
	conn.SetWriter(uploadBuf)
	conn.SetOnClose(func() {
		release()
		uploadBuf.Interrupt()
	})

	go d.uploadLoop(ctx, uploadBuf, maxUploadSize, scMinPostsIntervalMs, sessionId, httpClient, xmuxClient)

	return conn, nil
}

func (d *Dialer) uploadLoop(ctx context.Context, buf *pipebuffer.Buffer, maxUploadSize int32, interval config.Range, sessionId string, httpClient DialerClient, xmuxClient *XmuxClient) {
	var seq int64
	var lastWrite time.Time
	url := d.requestURL

	for {
		remainder, err := buf.ReadChunk(int(maxUploadSize))
		if err != nil {
			return
		}

		for len(remainder) > 0 {
			n := len(remainder)
			if n > int(maxUploadSize) {
				n = int(maxUploadSize)
			}
			chunk := remainder[:n]
			remainder = remainder[n:]

			seqStr := strconv.FormatInt(seq, 10)
			seq++

			if interval.From > 0 {
				if wait := time.Duration(interval.Rand())*time.Millisecond - time.Since(lastWrite); wait > 0 {
					time.Sleep(wait)
				}
			}
			lastWrite = time.Now()

			if xmuxClient != nil && (xmuxClient.LeftRequests.Add(-1) <= 0 ||
				(xmuxClient.UnreusableAt != time.Time{} && lastWrite.After(xmuxClient.UnreusableAt))) {
				httpClient, xmuxClient = d.getHTTPClient(ctx)
			}

			if err := httpClient.PostPacket(ctx, url, sessionId, seqStr, chunk); err != nil {
				logger.Printf("xhttp: failed to send upload: %v", err)
				buf.Interrupt()
				return
			}
		}
	}
}

func (d *Dialer) init() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.initialized {
		return
	}
	d.initialized = true
	d.httpVersion = decideHTTPVersion(d.TLSConfig)

	u := url.URL{}
	if d.TLSConfig != nil {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	u.Host = d.urlHost()
	u.Path = d.Config.GetNormalizedPath()
	u.RawQuery = d.Config.GetNormalizedQuery()
	d.requestURL = u.String()
}

func (d *Dialer) getHTTPClient(ctx context.Context) (DialerClient, *XmuxClient) {
	d.mu.Lock()
	if d.xmuxManager == nil {
		xmuxConfig := d.Config.GetNormalizedXmux()
		d.xmuxManager = NewXmuxManager(xmuxConfig, func() XmuxConn {
			return d.createHTTPClient()
		})
	}
	manager := d.xmuxManager
	d.mu.Unlock()

	xmuxClient := manager.GetXmuxClient(ctx)
	return xmuxClient.XmuxConn.(DialerClient), xmuxClient
}

func (d *Dialer) createHTTPClient() DialerClient {
	tlsConfig := d.TLSConfig
	httpVersion := decideHTTPVersion(tlsConfig)

	var gotlsConfig *tls.Config
	if tlsConfig != nil {
		gotlsConfig = tlsConfig.Clone()
		if gotlsConfig.ServerName == "" {
			gotlsConfig.ServerName = d.tlsServerName()
		}
	}

	dialTLS := func(ctx context.Context) (net.Conn, error) {
		conn, err := d.dialRawConn(ctx)
		if err != nil {
			return nil, err
		}
		if gotlsConfig != nil {
			tconn := tls.Client(conn, gotlsConfig)
			if err := tconn.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, err
			}
			conn = tconn
		}
		return conn, nil
	}

	var transport http.RoundTripper
	if httpVersion == "2" {
		// ForceAttemptHTTP2 makes net/http use its bundled HTTP/2 support even
		// though we supply a custom DialContext. The transport performs the TLS
		// handshake itself so that ALPN can select h2.
		//
		// Note: xray-core additionally sets http2.ReadIdleTimeout (the Chrome
		// keep-alive of ~45s) here, which requires golang.org/x/net/http2. It is
		// omitted so that this package stays on the standard library; net/http's
		// bundled HTTP/2 client has no equivalent knob.
		transport = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.dialRawConn(ctx)
			},
			TLSClientConfig:   gotlsConfig,
			ForceAttemptHTTP2: true,
			IdleConnTimeout:   connIdleTimeout,
		}
	} else {
		httpDialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialTLS(ctx)
		}
		transport = &http.Transport{
			DialTLSContext:  httpDialContext,
			DialContext:     httpDialContext,
			IdleConnTimeout: connIdleTimeout,
			// Chunked transfer download with keep-alives is unreliable for the
			// long-lived downlink; xhttp deliberately opens a fresh connection.
			DisableKeepAlives: true,
		}
	}

	return &DefaultDialerClient{
		transportConfig: d.Config,
		client:          &http.Client{Transport: transport},
		httpVersion:     httpVersion,
		uploadRawPool:   &sync.Pool{},
		dialUploadConn:  dialTLS,
	}
}

func (d *Dialer) dialRawConn(ctx context.Context) (net.Conn, error) {
	if d.RawDialContext != nil {
		return d.RawDialContext(ctx, "tcp", d.Address)
	}
	nd := &net.Dialer{Timeout: d.Timeout}
	return nd.DialContext(ctx, "tcp", d.Address)
}

func (d *Dialer) urlHost() string {
	if d.Host != "" {
		return d.Host
	}
	if d.Config != nil && d.Config.Host != "" {
		return d.Config.Host
	}
	if d.TLSConfig != nil && d.TLSConfig.ServerName != "" {
		return d.TLSConfig.ServerName
	}
	if host, _, err := net.SplitHostPort(d.Address); err == nil {
		return host
	}
	return d.Address
}

func (d *Dialer) tlsServerName() string {
	host := d.urlHost()
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

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

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
