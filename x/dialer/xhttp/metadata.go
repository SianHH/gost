package xhttp

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	xhutil "github.com/go-gost/x/internal/util/xhttp"
	mdutil "github.com/go-gost/x/metadata/util"
	xhconfig "xhttp/config"
)

type metadata struct {
	host   string
	config *xhconfig.Config

	tcpKeepalive         bool
	tcpKeepaliveIdle     time.Duration
	tcpKeepaliveInterval time.Duration
	tcpKeepaliveCount    int
}

func (d *xhttpDialer) parseMetadata(md mdata.Metadata) (err error) {
	d.md.host = mdutil.GetString(md, "xhttp.host", "host")
	d.md.config = xhutil.ParseConfig(md)

	d.md.tcpKeepalive = mdutil.GetBool(md, "tcp.keepalive")
	d.md.tcpKeepaliveIdle = mdutil.GetDuration(md, "tcp.keepalive.idle")
	d.md.tcpKeepaliveInterval = mdutil.GetDuration(md, "tcp.keepalive.interval")
	d.md.tcpKeepaliveCount = mdutil.GetInt(md, "tcp.keepalive.count")

	return
}
