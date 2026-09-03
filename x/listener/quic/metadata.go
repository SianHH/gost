package quic

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	"github.com/go-gost/x/internal/util/congestion/bbr"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	defaultBacklog = 128
)

type metadata struct {
	// keepAlive        bool
	keepAlivePeriod  time.Duration
	handshakeTimeout time.Duration
	maxIdleTimeout   time.Duration
	maxStreams       int
	enableDatagram   bool

	cipherKey []byte
	backlog   int

	tx             int
	txCompensation bool
	bbrLevel       bbr.Profile
}

func (l *quicListener) parseMetadata(md mdata.Metadata) (err error) {
	const (
		keepAlive        = "keepAlive"
		keepAlivePeriod  = "ttl"
		handshakeTimeout = "handshakeTimeout"
		maxIdleTimeout   = "maxIdleTimeout"
		maxStreams       = "maxStreams"

		backlog   = "backlog"
		cipherKey = "cipherKey"

		tx             = "tx"             // 固定速率
		txCompensation = "txCompensation" // 是否使用补偿
		bbrLevel       = "bbrLevel"       // bbr激进等级
	)

	l.md.backlog = mdutil.GetInt(md, backlog)
	if l.md.backlog <= 0 {
		l.md.backlog = defaultBacklog
	}

	if key := mdutil.GetString(md, cipherKey); key != "" {
		l.md.cipherKey = []byte(key)
	}

	if md == nil || !md.IsExists(keepAlive) || mdutil.GetBool(md, keepAlive) {
		l.md.keepAlivePeriod = mdutil.GetDuration(md, keepAlivePeriod)
		if l.md.keepAlivePeriod <= 0 {
			l.md.keepAlivePeriod = 10 * time.Second
		}
	}
	l.md.handshakeTimeout = mdutil.GetDuration(md, handshakeTimeout)
	l.md.maxIdleTimeout = mdutil.GetDuration(md, maxIdleTimeout)
	if l.md.maxIdleTimeout <= 0 {
		l.md.maxIdleTimeout = 90 * time.Second
	}
	l.md.maxStreams = mdutil.GetInt(md, maxStreams)
	l.md.enableDatagram = mdutil.GetBool(md, "quic.enableDatagram", "enableDatagram")

	l.md.tx = mdutil.GetInt(md, tx)
	l.md.txCompensation = mdutil.GetBool(md, txCompensation)

	switch mdutil.GetInt(md, bbrLevel) {
	case 0:
		l.md.bbrLevel = bbr.ProfileConservative
	case 1:
		l.md.bbrLevel = bbr.ProfileStandard
	case 2:
		l.md.bbrLevel = bbr.ProfileAggressive
	default:
		l.md.bbrLevel = bbr.ProfileConservative
	}
	return
}
