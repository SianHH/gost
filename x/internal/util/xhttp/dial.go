package xhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/go-gost/core/logger"
	"github.com/google/uuid"
)

// DialOptions carries everything needed to establish one XHTTP session on top
// of an existing (chain-dialed) base connection.
type DialOptions struct {
	// Config is the parsed XHTTP protocol configuration.
	Config *Config

	// BaseConn is the raw TCP connection produced by the gost chain Dial step.
	// It becomes the download connection of the session.
	BaseConn net.Conn

	// ExtraDial dials additional raw connections when the HTTP/1.1 mode needs
	// a separate upload connection. May be nil when only HTTP/2 is possible.
	ExtraDial func(ctx context.Context) (net.Conn, error)

	// TLSConfig is the client TLS configuration; nil selects plaintext HTTP.
	TLSConfig *tls.Config

	// URLHost is the value used for the URL host component (Host header /
	// TLS SNI). e.g. "example.com" or "example.com:8443".
	URLHost string

	// Logger is optional (may be nil).
	Logger logger.Logger
}

// Dial establishes an XHTTP session and returns a splitConn whose Read side is
// the download stream and whose Write side is the upload stream.
//
// Mode resolution ("auto"):
//   - over HTTP/2  -> "stream-up"
//   - over HTTP/1.1 -> "packet-up"
//
// (gost has no REALITY, so xray's REALITY-triggered "stream-one"/"stream-up"
// branches do not apply. "auto" over HTTP/1.1 resolves to packet-up because
// that is the reliable full-duplex mode for HTTP/1.1 in the reference
// implementation.)
func Dial(ctx context.Context, o DialOptions) (net.Conn, error) {
	scheme := "http"
	if o.TLSConfig != nil {
		scheme = "https"
	}
	requestURL := url.URL{
		Scheme:   scheme,
		Host:     o.URLHost,
		Path:     o.Config.GetNormalizedPath(),
		RawQuery: o.Config.GetNormalizedQuery(),
	}

	cli, err := newClient(ctx, o.Config, o.BaseConn, o.TLSConfig, o.ExtraDial, o.Logger)
	if err != nil {
		return nil, err
	}

	mode := o.Config.Mode
	if mode == "" || mode == "auto" {
		if cli.httpVersion == "2" {
			mode = "stream-up"
		} else {
			mode = "packet-up"
		}
	}
	if o.Logger != nil {
		o.Logger.Infof("xhttp dial %s mode=%s http=%s", requestURL.String(), mode, cli.httpVersion)
	}

	sessionID := ""
	if mode != "stream-one" {
		sessionID = uuid.New().String()
	}

	connCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	closeOnce := sync.Once{}
	closeCli := func() {
		closeOnce.Do(func() {
			cancel()
			cli.close()
		})
	}

	// The upload stream. In packet-up mode the writer is a bounded byte
	// buffer sized to one upload batch (up to scMaxEachPostBytes): small
	// application writes accumulate in it and are drained together into
	// ~1MiB POSTs by the packet-up pump, instead of each small write
	// generating its own round-trip HTTP request. Stream modes keep io.Pipe
	// (whose block-until-consumed semantics are what net/http's streaming
	// request-body writer expects for HTTP/1.1).
	var upReader io.Reader
	var upWriter uploadWriter
	if mode == "packet-up" {
		pr, pw := newMemPipe(uploadPipeCapacity(o.Config))
		upReader, upWriter = pr, pw
	} else {
		upReader, upWriter = io.Pipe()
	}

	baseLocal := o.BaseConn.LocalAddr()
	baseRemote := o.BaseConn.RemoteAddr()
	if baseLocal == nil {
		baseLocal = &net.TCPAddr{}
	}
	if baseRemote == nil {
		baseRemote = &net.TCPAddr{}
	}

	if mode == "stream-one" {
		// stream-one: a single request whose body is the upload stream and
		// whose response body is the download stream (works over HTTP/2).
		rc, err := cli.openStream(connCtx, o.Config.GetNormalizedUplinkHTTPMethod(), requestURL.String(), "", upReader)
		if err != nil {
			closeCli()
			return nil, err
		}

		return &splitConn{
			writer:     upWriter,
			reader:     rc,
			localAddr:  baseLocal,
			remoteAddr: baseRemote,
			onClose:    closeCli,
		}, nil
	}

	// stream-down: the long-lived download GET that keeps the session alive.
	downRC, err := cli.openStream(connCtx, http.MethodGet, requestURL.String(), sessionID, nil)
	if err != nil {
		closeCli()
		return nil, err
	}

	conn := &splitConn{
		reader:     downRC,
		writer:     upWriter,
		localAddr:  baseLocal,
		remoteAddr: baseRemote,
		onClose:    closeCli,
	}

	switch mode {
	case "stream-up":
		err = startStreamUp(connCtx, cli, upReader, upWriter, requestURL.String(), sessionID)
	case "packet-up":
		err = startPacketUp(connCtx, cancel, cli, upReader, upWriter, o.Config, requestURL.String(), sessionID)
	default:
		upWriter.Close()
		closeCli()
		return nil, fmt.Errorf("xhttp: unknown mode %q", mode)
	}
	if err != nil {
		upWriter.CloseWithError(err)
		closeCli()
		return nil, err
	}

	return conn, nil
}

// uploadWriter is the write end of the session upload stream; both io.Pipe
// and memPipe satisfy it.
type uploadWriter interface {
	io.Writer
	Close() error
	CloseWithError(err error) error
}

// startStreamUp opens the long-lived upload POST whose body is the write pipe.
// The request runs asynchronously: connection-level failures propagate to the
// pipe writer (and therefore to conn.Write). The response body (heartbeat 'X'
// bytes) is drained in the background.
func startStreamUp(ctx context.Context, cli *client, upReader io.Reader,
	upWriter uploadWriter, requestURL, sessionID string) error {

	go func() {
		rc, err := cli.openStream(ctx, cli.config.GetNormalizedUplinkHTTPMethod(), requestURL, sessionID, upReader)
		if err != nil {
			upWriter.CloseWithError(err)
			return
		}
		_, err = io.Copy(io.Discard, rc)
		rc.Close()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			upWriter.CloseWithError(err)
		}
	}()

	return nil
}

// maxUploadInflight caps how many packet-up POSTs may be concurrently in
// flight (launched but with their HTTP responses not yet consumed). Responses
// are drained by the background goroutines, so without a cap a high-latency
// link would accumulate an unbounded number of concurrent requests. The
// server reassembles out-of-order packets by sequence number with room for
// scMaxBufferedPosts (default 30), far above this depth.
const maxUploadInflight = 4

// startPacketUp launches the packet-up pump: it drains the buffered upload
// pipe (which batches many small application writes) and uploads each chunk
// as a separate POST carrying a monotonically increasing sequence number.
//
// Unlike the previous synchronous loop (one blocking request per read), POSTs
// are launched asynchronously so the response round trip of one packet does
// not delay the next packet. Only the pacing interval between posts and the
// maxUploadInflight bound throttle the launch rate, mirroring xray's packet-up
// loop. Returns an error only if the pump fails before it can start reading
// (never in practice).
func startPacketUp(ctx context.Context, cancel context.CancelFunc, cli *client,
	upReader io.Reader, upWriter uploadWriter, cfg *Config, requestURL, sessionID string) error {

	scMaxEachPostBytes := cfg.GetNormalizedScMaxEachPostBytes()
	maxPost := scMaxEachPostBytes.rand()
	if maxPost <= 0 {
		maxPost = 1
	}
	buf := make([]byte, maxPost)
	interval := cfg.GetNormalizedScMinPostsIntervalMs()
	inflight := make(chan struct{}, maxUploadInflight)

	go func() {
		var seq uint64
		var lastWrite time.Time

		// fail tears the whole session down on the first post error: a packet
		// that can never arrive would otherwise leave the server-side
		// reassembly queue waiting for it forever.
		fail := func(err error) {
			cancel()
			upWriter.CloseWithError(err)
		}

		for {
			n, err := upReader.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])

				if interval.From > 0 {
					d := time.Duration(interval.rand())*time.Millisecond - time.Since(lastWrite)
					if d > 0 {
						select {
						case <-time.After(d):
						case <-ctx.Done():
							fail(ctx.Err())
							return
						}
					}
				}
				lastWrite = time.Now()

				select {
				case inflight <- struct{}{}:
				case <-ctx.Done():
					fail(ctx.Err())
					return
				}

				seqStr := strconv.FormatUint(seq, 10)
				seq++

				go func(payload []byte, seqStr string) {
					defer func() { <-inflight }()
					if err := cli.postPacket(ctx, requestURL, sessionID, seqStr, payload); err != nil {
						fail(err)
					}
				}(payload, seqStr)
			}
			if err != nil {
				// A clean Close() surfaces as io.EOF once the buffered data is
				// drained; CloseWithError wakes us with the abort error.
				break
			}
		}
	}()

	return nil
}

// uploadPipeCapacity returns the byte capacity of the per-session upload
// buffer: enough for one full scMaxEachPostBytes-sized batch.
func uploadPipeCapacity(cfg *Config) int {
	if cfg == nil {
		return 1000000
	}
	return cfg.GetNormalizedScMaxEachPostBytes().To
}
