// manager.go — rotation modes. NOT in the supplied paste; the pasted
// structure lists manager.go but provides no code for it, and nothing in
// the paste reads cfg.Server.RotationMode. This file implements the two
// modes the spec orders.
//
// Per-session mode binds a proxy to a session id. A SOCKS5 client carries
// the session id as the SOCKS5 username (Chromium: --proxy-server=
// "socks5://<session-id>@127.0.0.1:1080"); the listener extracts it during
// username/password auth. Interval mode rotates one shared node on a timer.
package proxy

import (
	"sync"
	"time"
)

type Manager struct {
	pool     *ProxyPool
	mode     string
	interval time.Duration

	mu       sync.Mutex
	sessions map[string]*ProxyNode
	current  *ProxyNode
	setAt    time.Time
}

func NewManager(pool *ProxyPool, mode string, interval time.Duration) *Manager {
	if mode != "per_session" && mode != "interval" {
		mode = "per_session"
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Manager{
		pool:     pool,
		mode:     mode,
		interval: interval,
		sessions: make(map[string]*ProxyNode),
	}
}

// Pick returns the upstream node for a connection. sessionID is "" when the
// client sent no username (or when the client used plain HTTP proxying).
// A pinned session node that went unhealthy is re-picked immediately.
func (m *Manager) Pick(sessionID string) *ProxyNode {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.mode == "per_session" && sessionID != "" {
		if n, ok := m.sessions[sessionID]; ok && n != nil && n.IsHealthy.Load() {
			return n
		}
		n := m.pool.Next()
		if n != nil {
			m.sessions[sessionID] = n
		}
		return n
	}

	// interval mode (and per-session connections without a session id):
	// one shared node, rotated when it dies or the timer expires.
	if m.current == nil || !m.current.IsHealthy.Load() || time.Since(m.setAt) > m.interval {
		m.current = m.pool.Next()
		m.setAt = time.Now()
	}
	return m.current
}

// Mode reports the configured rotation mode.
func (m *Manager) Mode() string { return m.mode }
