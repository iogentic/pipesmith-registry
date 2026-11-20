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
	// the table holds MaxForwardedPeers peers; a new peer while it is full of
	// recent ones takes the place of the oldest and gets its line (review
	// 3-38)
	for i := range auth.MaxForwardedPeers + 10 {
		from(fmt.Sprintf("10.0.%d.%d", i/256, i%256), "X-Forwarded-For", canary)
	}
	if n := lines(); n != 3+auth.MaxForwardedPeers+10 {
		t.Fatalf("%d lines after %d new peers, want %d", n, auth.MaxForwardedPeers+10, 3+auth.MaxForwardedPeers+10)
	}
	if n := auth.ForwardedPeers(w); n != auth.MaxForwardedPeers {
		t.Fatalf("the table holds %d peers, want %d", n, auth.MaxForwardedPeers)
	}
	from("10.9.9.8", "X-Forwarded-For", canary)
	if n := lines(); n != 3+auth.MaxForwardedPeers+11 || !strings.Contains(logs.String(), "10.9.9.8") {
		t.Fatalf("a new peer after %d recent ones: %d lines", auth.MaxForwardedPeers, n)
	}
}

// Review 3-38: a real proxy that comes after MaxForwardedPeers other peers,
// all recent, gets its line; the table stays bounded, and the lines of one
// ForwardedWarnEvery are bounded by MaxForwardedLines plus one line that
// says lines were left out.
func TestForwardedWarningOfANewPeerAfterAFullTable(t *testing.T) {
	var logs bytes.Buffer
	c := newClock()
	w := auth.NewForwardedWarner(nil, slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil)), c.Now)
	from := func(peer string) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = peer + ":40000"
		r.Header.Set("X-Forwarded-For", "203.0.113.250")
		w.Check(r)
	}
	lines := func() int { return strings.Count(logs.String(), "\n") }
	for i := range auth.MaxForwardedPeers {
		from(fmt.Sprintf("10.1.%d.%d", i/256, i%256))
	}
	from("198.51.100.20") // the real proxy
	if !strings.Contains(logs.String(), "forwarded headers from 198.51.100.20,") || lines() != auth.MaxForwardedPeers+1 {
		t.Fatalf("the proxy after %d recent peers: %d lines, proxy named %v", auth.MaxForwardedPeers, lines(),
			strings.Contains(logs.String(), "198.51.100.20"))
	}
	if n := auth.ForwardedPeers(w); n > auth.MaxForwardedPeers {
		t.Fatalf("the table holds %d peers, bound %d", n, auth.MaxForwardedPeers)
	}
	for i := lines(); i < auth.MaxForwardedLines; i++ {
		from(fmt.Sprintf("10.2.%d.%d", i/256, i%256))
	}
	if lines() != auth.MaxForwardedLines {
		t.Fatalf("%d lines, want %d", lines(), auth.MaxForwardedLines)
	}
	from("198.51.100.21")
	from("198.51.100.22")
	if lines() != auth.MaxForwardedLines+1 || strings.Contains(logs.String(), "198.51.100.21") ||
		!strings.Contains(logs.String(), "lines left out") {
		t.Fatalf("beyond the bound of lines: %d lines, last %q", lines(), logs.String()[strings.LastIndex(strings.TrimSuffix(logs.String(), "\n"), "\n")+1:])
	}
	c.Add(auth.ForwardedWarnEvery)
	from("198.51.100.21")
	if lines() != auth.MaxForwardedLines+2 || !strings.Contains(logs.String(), "forwarded headers from 198.51.100.21,") {
		t.Fatalf("the peer left out, in the next interval: %d lines", lines())
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
