// Package splitconn provides the net.Conn implementation shared by the xhttp
// dialer and listener. A split connection reads from one stream and writes to
// another, which is exactly the shape of an xhttp session.
package splitconn

import (
	"io"
	"net"
	"time"
)

// Conn is a net.Conn whose read and write halves may come from different
// sources (for example, an HTTP response body and a queue of uploads).
type Conn struct {
	writer     io.WriteCloser
	reader     io.ReadCloser
	remoteAddr net.Addr
	localAddr  net.Addr
	onClose    func()
}

// New returns an empty Conn. Set the halves with the setters below before use.
func New() *Conn {
	return &Conn{}
}

// SetWriter sets the write half.
func (c *Conn) SetWriter(w io.WriteCloser) { c.writer = w }

// SetReader sets the read half.
func (c *Conn) SetReader(r io.ReadCloser) { c.reader = r }

// SetRemoteAddr sets the reported remote address.
func (c *Conn) SetRemoteAddr(a net.Addr) { c.remoteAddr = a }

// SetLocalAddr sets the reported local address.
func (c *Conn) SetLocalAddr(a net.Addr) { c.localAddr = a }

// SetOnClose registers a callback invoked once, when Close is called.
func (c *Conn) SetOnClose(f func()) { c.onClose = f }

// Write writes to the write half.
func (c *Conn) Write(b []byte) (int, error) { return c.writer.Write(b) }

// Read reads from the read half.
func (c *Conn) Read(b []byte) (int, error) { return c.reader.Read(b) }

// Close closes both halves, invoking the onClose callback first.
func (c *Conn) Close() error {
	if c.onClose != nil {
		c.onClose()
	}

	err := c.writer.Close()
	err2 := c.reader.Close()
	if err != nil {
		return err
	}
	return err2
}

// LocalAddr returns the local address, if any.
func (c *Conn) LocalAddr() net.Addr { return c.localAddr }

// RemoteAddr returns the remote address, if any.
func (c *Conn) RemoteAddr() net.Addr { return c.remoteAddr }

// SetDeadline is a no-op: xhttp has no useful notion of connection deadlines.
func (c *Conn) SetDeadline(t time.Time) error { return nil }

// SetReadDeadline is a no-op.
func (c *Conn) SetReadDeadline(t time.Time) error { return nil }

// SetWriteDeadline is a no-op.
func (c *Conn) SetWriteDeadline(t time.Time) error { return nil }
