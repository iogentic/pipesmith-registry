package auth

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Bounds of the warning of forwarded headers from a peer that is no trusted
// proxy (review D1).
const (
	ForwardedWarnEvery = 10 * time.Minute
	MaxForwardedPeers  = 100
	MaxForwardedLines  = 2 * MaxForwardedPeers
)

// forwardedHeaders are the headers a reverse proxy adds.
var forwardedHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"}

// ForwardedWarner writes a warning when a request carries forwarded headers
// from a peer that is no trusted proxy. Such a peer is a reverse proxy that
// trusted_proxies does not list, whose clients the sign in limits count as
// one, or a client that names an address of its choice. A peer gets at most
// one line per ForwardedWarnEvery. At most MaxForwardedPeers peers are
// remembered: a new peer while the table is full of recent ones takes the
// place of the peer whose line is oldest and gets its line (review 3-38).
// The lines of one interval are bounded by MaxForwardedLines; the first peer
// beyond them gets one line that says lines were left out, and a peer left
// out gets its line in a later interval. The line names the IP address of
// the peer and no header value; a peer without one gets none.
type ForwardedWarner struct {
	proxies TrustedProxies
	log     *slog.Logger
	now     func() time.Time
	mu      sync.Mutex
	seen    map[string]time.Time
	window  time.Time // the start of the interval the lines are counted in
	lines   int
	muted   bool // the line about lines left out was written in this interval
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
	line, muted := w.due(host)
	if muted {
		w.log.Warn("forwarded headers from more peers that are no trusted proxy than " + strconv.Itoa(MaxForwardedLines) +
			" lines allow in " + ForwardedWarnEvery.String() + ": lines left out until the interval ends")
	}
	if !line {
		return
	}
	// #nosec G706 -- host is the IP address of the connection as netip prints it, no text of the request.
	w.log.Warn("forwarded headers from " + host + ", which is no trusted proxy: the sign in limits and the session cookie " +
		"take the address of the connection. If " + host + " is a reverse proxy, list it in trusted_proxies of config.json")
}

// due marks a line for host and reports whether one is due now, and
// whether the line about lines left out is due instead.
func (w *ForwardedWarner) due(host string) (line, muted bool) {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	last, held := w.seen[host]
	if held && now.Sub(last) < ForwardedWarnEvery {
		return false, false
	}
	if now.Sub(w.window) >= ForwardedWarnEvery || now.Before(w.window) {
		w.window, w.lines, w.muted = now, 0, false
	}
	if w.lines >= MaxForwardedLines {
		first := !w.muted
		w.muted = true
		return false, first
	}
	if !held && len(w.seen) >= MaxForwardedPeers {
		oldest, oldestAt, found := "", time.Time{}, false
		for p, at := range w.seen {
			if now.Sub(at) >= ForwardedWarnEvery {
				delete(w.seen, p)
				continue
			}
			if !found || at.Before(oldestAt) {
				oldest, oldestAt, found = p, at, true
			}
		}
		if len(w.seen) >= MaxForwardedPeers && found {
			delete(w.seen, oldest)
		}
	}
	w.seen[host] = now
	w.lines++
	return true, false
}
