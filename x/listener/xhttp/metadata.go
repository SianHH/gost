package xhttp

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	xhutil "github.com/go-gost/x/internal/util/xhttp"
	mdutil "github.com/go-gost/x/metadata/util"
	xhconfig "xhttp/config"
)

type metadata struct {
	config *xhconfig.Config

	mptcp bool

	tcpKeepalive         bool
	tcpKeepaliveIdle     time.Duration
	tcpKeepaliveInterval time.Duration
	tcpKeepaliveCount    int
}

func (l *xhttpListener) parseMetadata(md mdata.Metadata) (err error) {
	l.md.config = xhutil.ParseConfig(md)

	l.md.mptcp = mdutil.GetBool(md, "mptcp")

	l.md.tcpKeepalive = mdutil.GetBool(md, "tcp.keepalive")
	l.md.tcpKeepaliveIdle = mdutil.GetDuration(md, "tcp.keepalive.idle")
	l.md.tcpKeepaliveInterval = mdutil.GetDuration(md, "tcp.keepalive.interval")
	l.md.tcpKeepaliveCount = mdutil.GetInt(md, "tcp.keepalive.count")

	return
}
