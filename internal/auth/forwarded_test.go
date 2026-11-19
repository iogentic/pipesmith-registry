package auth_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
)

// Review D1: a request with forwarded headers from a peer that is no trusted
// proxy logs one warning that names the peer, at most once per peer in
// auth.ForwardedWarnEvery, with a bounded table of peers, and no header
// value.
func TestForwardedHeadersFromAnUntrustedPeer(t *testing.T) {
	var logs bytes.Buffer
	c := newClock()
	w := auth.NewForwardedWarner(auth.TrustedProxies{netip.MustParsePrefix("198.51.100.0/24")},
		slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil)), c.Now)
	const canary = "203.0.113.250"
	from := func(peer string, headers ...string) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = peer + ":40000"
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		w.Check(r)
	}
	lines := func() int { return strings.Count(logs.String(), "\n") }

	from("192.0.2.1", "X-Forwarded-For", canary)
	from("192.0.2.1", "X-Forwarded-Proto", "https")
	from("192.0.2.1", "Forwarded", "for="+canary)
	if lines() != 1 || !strings.Contains(logs.String(), "192.0.2.1") || !strings.Contains(logs.String(), "trusted_proxies") ||
		!strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), canary) {
		t.Fatalf("after three requests of one peer: %q", logs.String())
	}
	// no line for a trusted proxy or for a request without forwarded headers
	from("198.51.100.7", "X-Forwarded-For", canary)
	from("192.0.2.2")
	if lines() != 1 {
		t.Fatalf("a trusted proxy or a plain request wrote a line: %q", logs.String())
	}
	for _, h := range []string{"X-Forwarded-Host", "X-Real-Ip"} {
		from("192.0.2.3", h, canary)
		c.Add(auth.ForwardedWarnEvery)
	}
	if lines() != 3 || strings.Contains(logs.String(), canary) {
		t.Fatalf("one line per peer and interval: %q", logs.String())
	}
	// the table holds MaxForwardedPeers peers: the two peers above are old by
	// now and make room; a new peer while the table is full of recent ones
	// gets no line, and room comes back once they are old
	for i := range auth.MaxForwardedPeers + 10 {
		from(fmt.Sprintf("10.0.%d.%d", i/256, i%256), "X-Forwarded-For", canary)
	}
	if n := lines(); n != 3+auth.MaxForwardedPeers {
		t.Fatalf("%d lines after %d new peers, want %d", n, auth.MaxForwardedPeers+10, 3+auth.MaxForwardedPeers)
	}
	c.Add(auth.ForwardedWarnEvery - time.Second)
	from("10.9.9.8", "X-Forwarded-For", canary)
	c.Add(time.Second)
	from("10.9.9.9", "X-Forwarded-For", canary)
	if n := lines(); n != 3+auth.MaxForwardedPeers+1 || !strings.Contains(logs.String(), "10.9.9.9") || strings.Contains(logs.String(), "10.9.9.8") {
		t.Fatalf("a new peer before and after the table aged: %d lines", n)
	}
}

// The warning names only the IP address of the connection: a peer that has
// none, as a listener of another kind gives, writes no line.
func TestForwardedWarningNamesOnlyAnAddress(t *testing.T) {
	var logs bytes.Buffer
	c := newClock()
	w := auth.NewForwardedWarner(nil, slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil)), c.Now)
	for _, remote := range []string{"@demo-socket", "demo-host:40000", "[::ffff:192.0.2.9]:40000"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-For", "203.0.113.250")
		w.Check(r)
	}
	if got := logs.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "forwarded headers from 192.0.2.9,") ||
		strings.Contains(got, "demo") {
		t.Fatalf("log %q", got)
	}
}
