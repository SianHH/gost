package congestions

import (
	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/congestions/bbr"
	"github.com/apernet/quic-go/congestions/brutal"
)

func UseBBR(conn *quic.Conn) {
	conn.SetCongestionControl(bbr.NewBbrSender(
		bbr.DefaultClock{},
		bbr.GetInitialPacketSize(conn.RemoteAddr()),
	))
}

func UseBrutal(conn *quic.Conn, tx uint64) {
	conn.SetCongestionControl(brutal.NewBrutalSender(tx))
}
