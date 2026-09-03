package quic

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	"github.com/go-gost/x/internal/util/congestion/bbr"
	mdutil "github.com/go-gost/x/metadata/util"
)

type metadata struct {
	keepAlivePeriod  time.Duration
	maxIdleTimeout   time.Duration
	handshakeTimeout time.Duration
	maxStreams       int
	enableDatagram   bool

	cipherKey []byte

	tx             int
	txCompensation bool
	bbrLevel       bbr.Profile
}

func (d *quicDialer) parseMetadata(md mdata.Metadata) (err error) {
	const (
		keepAlive        = "keepAlive"
		keepAlivePeriod  = "ttl"
		handshakeTimeout = "handshakeTimeout"
		maxIdleTimeout   = "maxIdleTimeout"
		maxStreams       = "maxStreams"

		cipherKey = "cipherKey"

		tx             = "tx"             // 固定速率
		txCompensation = "txCompensation" // 是否使用补偿
		bbrLevel       = "bbrLevel"       // bbr激进等级
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
	if d.md.maxIdleTimeout <= 0 {
		d.md.maxIdleTimeout = 90 * time.Second
	}
	d.md.maxStreams = mdutil.GetInt(md, maxStreams)
	d.md.enableDatagram = mdutil.GetBool(md, "quic.enableDatagram", "enableDatagram")

	d.md.tx = mdutil.GetInt(md, tx)
	d.md.txCompensation = mdutil.GetBool(md, txCompensation)

	switch mdutil.GetInt(md, bbrLevel) {
	case 0:
		d.md.bbrLevel = bbr.ProfileConservative
	case 1:
		d.md.bbrLevel = bbr.ProfileStandard
	case 2:
		d.md.bbrLevel = bbr.ProfileAggressive
	default:
		d.md.bbrLevel = bbr.ProfileConservative
	}
	return
}
