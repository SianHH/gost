package dialer

import (
	"bufio"
	"net"
)

// H1Conn wraps a raw connection used for HTTP/1.1 uploads so that responses of
// pipelined requests can be read back in order.
type H1Conn struct {
	UnreadedResponsesCount int
	RespBufReader          *bufio.Reader
	net.Conn
}

// NewH1Conn wraps conn.
func NewH1Conn(conn net.Conn) *H1Conn {
	return &H1Conn{
		RespBufReader: bufio.NewReader(conn),
		Conn:          conn,
	}
}
