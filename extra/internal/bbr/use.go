package bbr

import "github.com/apernet/quic-go"

func UseBBR(conn quic.Connection) {
	conn.SetCongestionControl(NewBbrSender(
		DefaultClock{},
		GetInitialPacketSize(conn.RemoteAddr()),
	))
}
