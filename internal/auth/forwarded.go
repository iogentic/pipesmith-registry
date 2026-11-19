package auth

import (
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Bounds of the warning of forwarded headers from a peer that is no trusted
// proxy (review D1).
const (
	ForwardedWarnEvery = 10 * time.Minute
	MaxForwardedPeers  = 100
)

// forwardedHeaders are the headers a reverse proxy adds.
var forwardedHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"}

// ForwardedWarner writes a warning when a request carries forwarded headers
// from a peer that is no trusted proxy. Such a peer is a reverse proxy that
// trusted_proxies does not list, whose clients the sign in limits count as
// one, or a client that names an address of its choice. A peer gets at most
// one line per ForwardedWarnEvery; at most MaxForwardedPeers peers are
// remembered, and a new peer while the table is full of recent ones gets no
// line. The line names the IP address of the peer and no header value; a
// peer without one gets none.
type ForwardedWarner struct {
	proxies TrustedProxies
	log     *slog.Logger
	now     func() time.Time
	mu      sync.Mutex
	seen    map[string]time.Time
}

// NewForwardedWarner gives the warner of the trusted proxies, which writes to
// log; now is the clock.
func NewForwardedWarner(proxies TrustedProxies, log *slog.Logger, now func() time.Time) *ForwardedWarner {
	return &ForwardedWarner{proxies: proxies, log: log, now: now, seen: map[string]time.Time{}}
}

// Check writes the warning for r when it is due.
func (w *ForwardedWarner) Check(r *http.Request) {
	forwarded := false
	for _, h := range forwardedHeaders {
		forwarded = forwarded || len(r.Header.Values(h)) > 0
	}
	_, ip, ok := peer(r)
	if !forwarded || !ok || w.proxies.trusts(ip) {
		return
	}
	host := ip.String()
	if !w.due(host) {
		return
	}
	// #nosec G706 -- host is the IP address of the connection as netip prints it, no text of the request.
	w.log.Warn("forwarded headers from " + host + ", which is no trusted proxy: the sign in limits and the session cookie " +
		"take the address of the connection. If " + host + " is a reverse proxy, list it in trusted_proxies of config.json")
}

// due marks a line for host and reports whether one is due now.
func (w *ForwardedWarner) due(host string) bool {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, held := w.seen[host]; held {
		if now.Sub(last) < ForwardedWarnEvery {
			return false
		}
	} else if len(w.seen) >= MaxForwardedPeers {
		for p, at := range w.seen {
			if now.Sub(at) >= ForwardedWarnEvery {
				delete(w.seen, p)
			}
		}
		if len(w.seen) >= MaxForwardedPeers {
			return false
		}
	}
	w.seen[host] = now
	return true
}
