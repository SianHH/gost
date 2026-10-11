package dialer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"

	"xhttp/config"
	"xhttp/internal/done"
	"xhttp/internal/logger"
)

// DialerClient abstracts how xhttp requests are actually carried. The only
// implementation shipped here is DefaultDialerClient, built on net/http.
type DialerClient interface {
	IsClosed() bool

	// OpenStream opens the long-lived downlink (and, when body is non-nil, a
	// stream-up/stream-one uplink) request. It returns the response body to
	// read from, plus the local and remote addresses once the connection is up.
	OpenStream(ctx context.Context, url string, sessionId string, body io.Reader, uploadOnly bool) (io.ReadCloser, net.Addr, net.Addr, error)

	// PostPacket sends a single packet-up POST carrying payload.
	PostPacket(ctx context.Context, url string, sessionId string, seqStr string, payload []byte) error
}

// DefaultDialerClient implements DialerClient using net/http.
type DefaultDialerClient struct {
	transportConfig *config.Config
	client          *http.Client
	closed          atomic.Bool
	httpVersion     string
	// pool of net.Conn, created using dialUploadConn
	uploadRawPool  *sync.Pool
	dialUploadConn func(ctx context.Context) (net.Conn, error)
}

// IsClosed reports whether this client has hit a fatal error.
func (c *DefaultDialerClient) IsClosed() bool {
	return c.closed.Load()
}

// OpenStream implements DialerClient.
func (c *DefaultDialerClient) OpenStream(ctx context.Context, url string, sessionId string, body io.Reader, uploadOnly bool) (wrc io.ReadCloser, remoteAddr, localAddr net.Addr, err error) {
	gotConn := done.New()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(connInfo httptrace.GotConnInfo) {
			remoteAddr = connInfo.Conn.RemoteAddr()
			localAddr = connInfo.Conn.LocalAddr()
			gotConn.Close()
		},
	})

	method := "GET" // stream-down
	if body != nil {
		method = c.transportConfig.GetNormalizedUplinkHTTPMethod() // stream-up/one
	}
	req, _ := http.NewRequestWithContext(context.WithoutCancel(ctx), method, url, body)
	c.transportConfig.FillStreamRequest(req, sessionId, "")

	wrc = &WaitReadCloser{wait: done.New()}
	go func() {
		resp, err := c.client.Do(req)
		if err != nil {
			if !uploadOnly { // stream-down is enough
				c.closed.Store(true)
				logger.Printf("xhttp: failed to %s %s: %v", method, url, err)
			}
			gotConn.Close()
			wrc.Close()
			return
		}
		if resp.StatusCode != 200 && !uploadOnly {
			logger.Printf("xhttp: unexpected status %d", resp.StatusCode)
		}
		if resp.StatusCode != 200 || uploadOnly { // stream-up
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			wrc.Close()
			return
		}
		wrc.(*WaitReadCloser).Set(resp.Body)
	}()

	<-gotConn.Wait()
	return
}

// PostPacket implements DialerClient.
func (c *DefaultDialerClient) PostPacket(ctx context.Context, url string, sessionId string, seqStr string, payload []byte) error {
	method := c.transportConfig.GetNormalizedUplinkHTTPMethod()
	req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), method, url, nil)
	if err != nil {
		return err
	}
	c.transportConfig.FillPacketRequest(req, sessionId, seqStr, payload)

	if c.httpVersion != "1.1" {
		resp, err := c.client.Do(req)
		if err != nil {
			c.closed.Store(true)
			return err
		}
		io.Copy(io.Discard, resp.Body)
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return fmt.Errorf("bad status code: %s", resp.Status)
		}
		return nil
	}

	// Serialize the whole HTTP/1.1 request so it can be safely retried: calling
	// req.Write multiple times would fail because the body is already drained.
	requestBuff := new(bytes.Buffer)
	requestBuff.Grow(512 + int(req.ContentLength))
	if err := req.Write(requestBuff); err != nil {
		return err
	}

	var uploadConn any
	var h1UploadConn *H1Conn

	for {
		uploadConn = c.uploadRawPool.Get()
		newConnection := uploadConn == nil
		if newConnection {
			newConn, err := c.dialUploadConn(context.WithoutCancel(ctx))
			if err != nil {
				return err
			}
			h1UploadConn = NewH1Conn(newConn)
			uploadConn = h1UploadConn
		} else {
			h1UploadConn = uploadConn.(*H1Conn)

			// Drain the response of the previous pipelined request before
			// reusing this connection, otherwise responses accumulate in the
			// kernel receive buffer and the connection eventually stalls.
			if h1UploadConn.UnreadedResponsesCount > 0 {
				resp, err := http.ReadResponse(h1UploadConn.RespBufReader, req)
				if err != nil {
					c.closed.Store(true)
					return fmt.Errorf("error while reading response: %w", err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					return fmt.Errorf("got non-200 error response code: %d", resp.StatusCode)
				}
				h1UploadConn.UnreadedResponsesCount--
			}
		}

		_, err := h1UploadConn.Write(requestBuff.Bytes())
		// A failed write on a pooled connection is normal when the connection
		// was closed in the meantime; retry with another connection until a
		// fresh connection also fails.
		if err == nil {
			h1UploadConn.UnreadedResponsesCount++
			break
		} else if newConnection {
			return err
		}
	}

	c.uploadRawPool.Put(uploadConn)
	return nil
}

// WaitReadCloser is an io.ReadCloser whose underlying reader is attached
// asynchronously; reads block until it is set or the closer is closed.
type WaitReadCloser struct {
	wait   *done.Instance
	reader atomic.Pointer[io.ReadCloser]
}

// Set attaches the underlying reader.
func (w *WaitReadCloser) Set(rc io.ReadCloser) {
	w.reader.Store(&rc)
	if w.wait.Done() {
		if p := w.reader.Swap(nil); p != nil {
			(*p).Close()
		}
	}
	w.wait.Close()
}

// Read implements io.Reader.
func (w *WaitReadCloser) Read(b []byte) (int, error) {
	rc := w.reader.Load()
	if rc == nil {
		<-w.wait.Wait()
		if rc = w.reader.Load(); rc == nil {
			return 0, io.ErrClosedPipe
		}
	}
	return (*rc).Read(b)
}

// Close implements io.Closer.
func (w *WaitReadCloser) Close() error {
	w.wait.Close()
	if p := w.reader.Swap(nil); p != nil {
		return (*p).Close()
	}
	return nil
}
