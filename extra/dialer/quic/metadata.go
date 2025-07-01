package quic

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
)

type metadata struct {
	keepAlivePeriod  time.Duration
	maxIdleTimeout   time.Duration
	handshakeTimeout time.Duration
	maxStreams       int
	enableDatagram   bool

	tx   string // 最大发送速率
	obfs string // 混淆

	cipherKey []byte
}

func (d *quicDialer) parseMetadata(md mdata.Metadata) (err error) {
	const (
		keepAlive        = "keepAlive"
		keepAlivePeriod  = "ttl"
		handshakeTimeout = "handshakeTimeout"
		maxIdleTimeout   = "maxIdleTimeout"
		maxStreams       = "maxStreams"

		cipherKey = "cipherKey"

		tx   = "tx"
		obfs = "obfs"
	)

	if key := mdutil.GetString(md, cipherKey); key != "" {
		d.md.cipherKey = []byte(key)
	}

	if md == nil || !md.IsExists(keepAlive) || mdutil.GetBool(md, keepAlive) {
		d.md.keepAlivePeriod = mdutil.GetDuration(md, keepAlivePeriod)
		if d.md.keepAlivePeriod <= 0 {
			d.md.keepAlivePeriod = 10 * time.Second
		}
	}
	d.md.handshakeTimeout = mdutil.GetDuration(md, handshakeTimeout)
	d.md.maxIdleTimeout = mdutil.GetDuration(md, maxIdleTimeout)
	d.md.maxStreams = mdutil.GetInt(md, maxStreams)
	d.md.enableDatagram = mdutil.GetBool(md, "quic.enableDatagram", "enableDatagram")
	d.md.tx = mdutil.GetString(md, tx)
	d.md.obfs = mdutil.GetString(md, obfs)
	return
}
