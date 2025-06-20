package brutal

import "github.com/apernet/quic-go"

func UseBrutal(conn quic.Connection, tx uint64) {
	conn.SetCongestionControl(NewBrutalSender(tx))
}
