package hy2

import (
	"net"

	"github.com/apernet/quic-go"
)

type quicConn struct {
	*quic.Stream
	laddr net.Addr
	raddr net.Addr
}

func (c *quicConn) LocalAddr() net.Addr {
	return c.laddr
}

func (c *quicConn) RemoteAddr() net.Addr {
	return c.raddr
}
