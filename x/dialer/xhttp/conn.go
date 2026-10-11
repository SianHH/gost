package xhttp

import "net"

// conn wraps an xhttp session connection so that LocalAddr and RemoteAddr are
// always non-nil. The xhttp dialer reports the addresses only after the
// underlying connection is established; if an open fails early they can be nil,
// and some GOST components assume a non-nil address when logging or deriving
// session metadata.
type conn struct {
	net.Conn
}

func (c *conn) LocalAddr() net.Addr {
	if addr := c.Conn.LocalAddr(); addr != nil {
		return addr
	}
	return &net.TCPAddr{}
}

func (c *conn) RemoteAddr() net.Addr {
	if addr := c.Conn.RemoteAddr(); addr != nil {
		return addr
	}
	return &net.TCPAddr{}
}
