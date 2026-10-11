package dialer

import (
	"context"
	"crypto/rand"
	"math"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"xhttp/config"
	"xhttp/internal/logger"
)

// XmuxConn is the part of a dialer client that the multiplexer needs to know about.
type XmuxConn interface {
	IsClosed() bool
}

// XmuxClient wraps one dialer client together with its usage counters.
type XmuxClient struct {
	XmuxConn     XmuxConn
	OpenUsage    atomic.Int32
	leftUsage    int32
	LeftRequests atomic.Int32
	UnreusableAt time.Time
}

// XmuxManager decides which XmuxClient a new session should use, enforcing the
// configured concurrency/connection limits and reuse policy.
type XmuxManager struct {
	mu          sync.Mutex
	xmuxConfig  config.XmuxConfig
	concurrency int32
	connections int32
	newConnFunc func() XmuxConn
	xmuxClients []*XmuxClient
}

// NewXmuxManager creates a manager that builds clients with newConnFunc.
func NewXmuxManager(xmuxConfig config.XmuxConfig, newConnFunc func() XmuxConn) *XmuxManager {
	return &XmuxManager{
		xmuxConfig:  xmuxConfig,
		concurrency: xmuxConfig.GetNormalizedMaxConcurrency().Rand(),
		connections: xmuxConfig.GetNormalizedMaxConnections().Rand(),
		newConnFunc: newConnFunc,
		xmuxClients: make([]*XmuxClient, 0),
	}
}

func (m *XmuxManager) newXmuxClient() *XmuxClient {
	xmuxClient := &XmuxClient{
		XmuxConn:  m.newConnFunc(),
		leftUsage: -1,
	}
	if x := m.xmuxConfig.GetNormalizedCMaxReuseTimes().Rand(); x > 0 {
		xmuxClient.leftUsage = x - 1
	}
	xmuxClient.LeftRequests.Store(math.MaxInt32)
	if x := m.xmuxConfig.GetNormalizedHMaxRequestTimes().Rand(); x > 0 {
		xmuxClient.LeftRequests.Store(x)
	}
	if x := m.xmuxConfig.GetNormalizedHMaxReusableSecs().Rand(); x > 0 {
		xmuxClient.UnreusableAt = time.Now().Add(time.Duration(x) * time.Second)
	}
	m.xmuxClients = append(m.xmuxClients, xmuxClient)
	return xmuxClient
}

// GetXmuxClient returns a client suitable for a new session.
func (m *XmuxManager) GetXmuxClient(ctx context.Context) *XmuxClient {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := 0; i < len(m.xmuxClients); {
		xmuxClient := m.xmuxClients[i]
		if xmuxClient.XmuxConn.IsClosed() ||
			xmuxClient.leftUsage == 0 ||
			xmuxClient.LeftRequests.Load() <= 0 ||
			(xmuxClient.UnreusableAt != time.Time{} && time.Now().After(xmuxClient.UnreusableAt)) {
			logger.Printf("XMUX: removing xmuxClient, IsClosed() = %v, OpenUsage = %d, leftUsage = %d, LeftRequests = %d, UnreusableAt = %v",
				xmuxClient.XmuxConn.IsClosed(),
				xmuxClient.OpenUsage.Load(),
				xmuxClient.leftUsage,
				xmuxClient.LeftRequests.Load(),
				xmuxClient.UnreusableAt)
			m.xmuxClients = append(m.xmuxClients[:i], m.xmuxClients[i+1:]...)
		} else {
			i++
		}
	}

	if len(m.xmuxClients) == 0 {
		logger.Printf("XMUX: creating xmuxClient because xmuxClients is empty")
		return m.newXmuxClient()
	}

	if m.connections > 0 && len(m.xmuxClients) < int(m.connections) {
		logger.Printf("XMUX: creating xmuxClient because maxConnections was not hit, xmuxClients = %d", len(m.xmuxClients))
		return m.newXmuxClient()
	}

	xmuxClients := make([]*XmuxClient, 0)
	if m.concurrency > 0 {
		for _, xmuxClient := range m.xmuxClients {
			if xmuxClient.OpenUsage.Load() < m.concurrency {
				xmuxClients = append(xmuxClients, xmuxClient)
			}
		}
	} else {
		xmuxClients = m.xmuxClients
	}

	if len(xmuxClients) == 0 {
		logger.Printf("XMUX: creating xmuxClient because maxConcurrency was hit, xmuxClients = %d", len(m.xmuxClients))
		return m.newXmuxClient()
	}

	i, _ := rand.Int(rand.Reader, big.NewInt(int64(len(xmuxClients))))
	xmuxClient := xmuxClients[i.Int64()]
	if xmuxClient.leftUsage > 0 {
		xmuxClient.leftUsage--
	}
	return xmuxClient
}
