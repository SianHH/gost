package xhttp

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	xhttp_util "github.com/go-gost/x/internal/util/xhttp"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	defaultPath    = "/"
	defaultBacklog = 128
)

type metadata struct {
	config  xhttp_util.Config
	backlog int
	mptcp   bool

	keepalive         bool
	keepaliveIdle     time.Duration
	keepaliveInterval time.Duration
	keepaliveCount    int
}

func (l *xhttpListener) parseMetadata(md mdata.Metadata) (err error) {
	l.md.config = xhttp_util.ConfigFromMetadata(md)
	if l.md.config.Path == "" {
		l.md.config.Path = defaultPath
	}

	l.md.backlog = mdutil.GetInt(md, "xhttp.backlog", "backlog")
	if l.md.backlog <= 0 {
		l.md.backlog = defaultBacklog
	}
	l.md.mptcp = mdutil.GetBool(md, "mptcp")

	l.md.keepalive = mdutil.GetBool(md, "keepalive")
	l.md.keepaliveIdle = mdutil.GetDuration(md, "keepalive.idle")
	l.md.keepaliveInterval = mdutil.GetDuration(md, "keepalive.interval")
	l.md.keepaliveCount = mdutil.GetInt(md, "keepalive.count")

	return
}
