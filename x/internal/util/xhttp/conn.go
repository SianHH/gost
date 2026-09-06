package xhttp

import (
	"io"
	"net"
	"sync"
	"time"
)

// splitConn is a net.Conn whose read side and write side travel over two
// separate HTTP streams (XHTTP: read = download response body, write = upload
// request body / packet POSTs). Ported from xray splithttp connection.go.
//
// Deadline handling is deliberately a no-op: HTTP streams provide no deadline
// semantics. Callers that need timeouts must rely on their own timers, exactly
// as xray does.
type splitConn struct {
	writer io.WriteCloser
	reader io.ReadCloser

	remoteAddr net.Addr
	localAddr  net.Addr

	onClose func()

	writeMu  sync.Mutex // serializes concurrent writes on the underlying writer
	closeMu  sync.Mutex
	closeErr error
	closed   bool
}

func (c *splitConn) Write(b []byte) (int, error) {
	if c.writer == nil {
		return 0, io.ErrClosedPipe
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writer.Write(b)
}

func (c *splitConn) Read(b []byte) (int, error) {
	if c.reader == nil {
		return 0, io.EOF
	}
	return c.reader.Read(b)
}

func (c *splitConn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return c.closeErr
	}
	c.closed = true

	if c.onClose != nil {
		c.onClose()
	}

	if c.writer != nil {
		c.closeErr = c.writer.Close()
	}
	if c.reader != nil {
		if err := c.reader.Close(); c.closeErr == nil {
			c.closeErr = err
		}
	}
	return c.closeErr
}

func (c *splitConn) LocalAddr() net.Addr {
	return c.localAddr
}

func (c *splitConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *splitConn) SetDeadline(t time.Time) error {
	// TODO: cannot do anything useful over HTTP streams.
	return nil
}

func (c *splitConn) SetReadDeadline(t time.Time) error {
	// TODO: cannot do anything useful over HTTP streams.
	return nil
}

func (c *splitConn) SetWriteDeadline(t time.Time) error {
	// TODO: cannot do anything useful over HTTP streams.
	return nil
}

// httpServerConn adapts a http.ResponseWriter/request body pair to the
// io.ReadWriteCloser contract used by the server side of a session. Writes
// land on the HTTP response (flushed immediately), reads consume the upload
// request body.
type httpServerConn struct {
	sync.Mutex
	done   chan struct{}
	reader io.Reader // request body; does not need to be closed
	writer io.Writer // http.ResponseWriter
}

func (c *httpServerConn) isDone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *httpServerConn) Write(b []byte) (int, error) {
	c.Lock()
	defer c.Unlock()
	if c.isDone() {
		return 0, io.ErrClosedPipe
	}
	n, err := c.writer.Write(b)
	if err == nil {
		if fw, ok := c.writer.(httpFlusher); ok {
			fw.Flush()
		}
	}
	return n, err
}

func (c *httpServerConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

func (c *httpServerConn) Close() error {
	c.Lock()
	defer c.Unlock()
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return nil
}

type httpFlusher interface {
	Flush()
}
