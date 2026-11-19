package auth_test

import (
	"bytes"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The address of a caller and the scheme of the session cookie (decision E8
// of the execution plan, N6 of the phase 3 plan, review focus 4): a forwarded
// header counts only from a peer in trusted_proxies, the client is the right
// most address of X-Forwarded-For that is no trusted proxy, and a value that
// is no address gives the address of the connection.

func proxyList(t testing.TB, texts ...string) auth.TrustedProxies {
	t.Helper()
	var out auth.TrustedProxies
	for _, text := range texts {
		p, err := netip.ParsePrefix(text)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func forwarded(peer, header string, values ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = peer
	for _, v := range values {
		r.Header.Add(header, v)
	}
	return r
}

// newEnvWith is newEnv with the options of the test; the clock and the log
// are those of newEnv.
func newEnvWith(t testing.TB, o auth.Options) *env {
	t.Helper()
	c := newClock()
	logs := &bytes.Buffer{}
	o.Now = c.Now
	o.Logger = slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, nil))
	svc, err := auth.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, svc: svc, handler: svc.SessionHandler(), clock: c, logs: logs}
}

func TestClientAddress(t *testing.T) {
	proxies := proxyList(t, "10.0.0.0/8", "2001:db8:1::/48", "192.0.2.7/32")
	cases := []struct {
		name      string
		peer      string
		forwarded []string
		want      string
	}{
		{"no header", "10.0.0.1:40000", nil, "10.0.0.1"},
		{"the peer is no trusted proxy", "198.51.100.4:40000", []string{"203.0.113.9"}, "198.51.100.4"},
		{"the neighbour of a trusted address", "192.0.2.8:40000", []string{"203.0.113.9"}, "192.0.2.8"},
		{"a peer without a port", "10.0.0.1", []string{"203.0.113.9"}, "10.0.0.1"},
		{"one proxy", "10.0.0.1:40000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"a trusted single address", "192.0.2.7:40000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"a chain", "10.0.0.1:40000", []string{"198.51.100.66, 203.0.113.9, 10.0.0.2"}, "203.0.113.9"},
		{"a chain over two lines", "10.0.0.1:40000", []string{"198.51.100.66, 203.0.113.9", "10.0.0.2"}, "203.0.113.9"},
		{"a chain without spaces", "10.0.0.1:40000", []string{"198.51.100.66,203.0.113.9,10.0.0.2"}, "203.0.113.9"},
		{"tabs around the entries", "10.0.0.1:40000", []string{"203.0.113.9\t,\t10.0.0.2"}, "203.0.113.9"},
		{"IPv6", "[2001:db8:1::5]:40000", []string{"2001:db8:2::9, 2001:db8:1::6"}, "2001:db8:2::9"},
		{"IPv4 in IPv6 form", "10.0.0.1:40000", []string{"::ffff:203.0.113.9"}, "203.0.113.9"},
		{"only trusted proxies", "10.0.0.1:40000", []string{"10.0.0.3, 10.0.0.2"}, "10.0.0.1"},
		{"no address", "10.0.0.1:40000", []string{"demo-not-an-address"}, "10.0.0.1"},
		{"no address at the right end", "10.0.0.1:40000", []string{"203.0.113.9, demo-not-an-address"}, "10.0.0.1"},
		{"no address after a trusted proxy", "10.0.0.1:40000", []string{"demo-not-an-address, 10.0.0.2"}, "10.0.0.1"},
		{"junk left of the client is not read", "10.0.0.1:40000", []string{"demo-not-an-address, 203.0.113.9"}, "203.0.113.9"},
		{"an empty entry", "10.0.0.1:40000", []string{"203.0.113.9, , 10.0.0.2"}, "10.0.0.1"},
		{"an empty value", "10.0.0.1:40000", []string{""}, "10.0.0.1"},
		{"an entry with a port", "10.0.0.1:40000", []string{"203.0.113.9:5555"}, "10.0.0.1"},
		{"an IPv6 entry in brackets", "10.0.0.1:40000", []string{"[2001:db8:2::9]"}, "10.0.0.1"},
		{"an entry with a zone", "10.0.0.1:40000", []string{"fe80::1%eth0"}, "10.0.0.1"},
		{"unknown", "10.0.0.1:40000", []string{"unknown"}, "10.0.0.1"},
		{"leading zeros", "10.0.0.1:40000", []string{"203.0.113.09"}, "10.0.0.1"},
	}
	for _, c := range cases {
		r := forwarded(c.peer, "X-Forwarded-For", c.forwarded...)
		if got := proxies.ClientAddress(r); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// The default list is empty: no forwarded header counts, from loopback
	// neither, where every client of an SSH tunnel comes from.
	var none auth.TrustedProxies
	for _, peer := range []string{"127.0.0.1:40000", "[::1]:40000", "10.0.0.1:40000"} {
		host, _, _ := net.SplitHostPort(peer)
		if got := none.ClientAddress(forwarded(peer, "X-Forwarded-For", "203.0.113.9")); got != host {
			t.Errorf("default list, peer %s: %q", peer, got)
		}
	}
}

func TestSecureScheme(t *testing.T) {
	proxies := proxyList(t, "10.0.0.0/8")
	cases := []struct {
		name  string
		peer  string
		tls   bool
		proto []string
		want  bool
	}{
		{"plain", "10.0.0.1:40000", false, nil, false},
		{"a TLS connection", "198.51.100.4:40000", true, nil, true},
		{"a trusted proxy says https", "10.0.0.1:40000", false, []string{"https"}, true},
		{"in upper case", "10.0.0.1:40000", false, []string{"HTTPS"}, true},
		{"a hop on the way said https", "10.0.0.1:40000", false, []string{"https, http"}, true},
		{"a trusted proxy says http", "10.0.0.1:40000", false, []string{"http"}, false},
		{"another scheme", "10.0.0.1:40000", false, []string{"demo-scheme"}, false},
		{"a peer that is no trusted proxy", "198.51.100.4:40000", false, []string{"https"}, false},
	}
	for _, c := range cases {
		r := forwarded(c.peer, "X-Forwarded-Proto", c.proto...)
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := proxies.Secure(r); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	var none auth.TrustedProxies
	if none.Secure(forwarded("127.0.0.1:40000", "X-Forwarded-Proto", "https")) {
		t.Error("default list: a forwarded https counts")
	}
}

// Review focus 4 through the sign in limits: behind a trusted proxy the pair
// is the user and the forwarded client, and a header from any other peer is
// ignored, so a client cannot leave its pair by naming another address.
func TestThrottleBehindTrustedProxy(t *testing.T) {
	e := newEnvWith(t, auth.Options{Snapshot: fixture(t), TrustedProxies: proxyList(t, "10.0.0.0/8")})
	via := func(peer, client, user, pass string) *httptest.ResponseRecorder {
		r := signInRequest(user, pass, peer)
		r.Header.Set("X-Forwarded-For", client)
		return e.do(r)
	}
	for i := range auth.FreeFailures {
		if rec := via("10.0.0.1", "203.0.113.9", "demo-admin", "demo-wrong-pass-1"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, rec.Code)
		}
	}
	if rec := via("10.0.0.2", "203.0.113.9", "demo-admin", password(t, "demo-admin")); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the same client through another proxy: %d, want 429", rec.Code)
	}
	if rec := via("10.0.0.1", "203.0.113.10", "demo-admin", password(t, "demo-admin")); rec.Code != http.StatusOK {
		t.Errorf("another client behind the same proxy: %d, want 200", rec.Code)
	}
	basicVia := func(client string) (*model.Subject, error) {
		r := basic(request(http.MethodGet, "/", nil), "demo-admin", password(t, "demo-admin"))
		r.RemoteAddr = "10.0.0.1:50000"
		r.Header.Set("X-Forwarded-For", client)
		return e.svc.Subject(r)
	}
	var me *model.Error
	if _, err := basicVia("203.0.113.9"); !errors.As(err, &me) || me.Status != http.StatusTooManyRequests {
		t.Errorf("Basic of the throttled client: %v", err)
	}
	if s, err := basicVia("203.0.113.11"); err != nil || s == nil {
		t.Errorf("Basic of another client: %v", err)
	}
	if got := e.svc.ClientAddress(forwarded("10.0.0.1:50000", "X-Forwarded-For", "203.0.113.12")); got != "203.0.113.12" {
		t.Errorf("ClientAddress of the service: %q", got)
	}

	// A peer that is no trusted proxy keeps its own address, whatever it names.
	for i := range auth.FreeFailures {
		client := netip.AddrFrom4([4]byte{203, 0, 113, byte(20 + i)}).String()
		if rec := via("198.51.100.4", client, "demo-reader", "demo-wrong-pass-1"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("untrusted peer, failure %d: %d", i+1, rec.Code)
		}
	}
	if rec := via("198.51.100.4", "203.0.113.40", "demo-reader", password(t, "demo-reader")); rec.Code != http.StatusTooManyRequests {
		t.Errorf("untrusted peer with a new forwarded address: %d, want 429", rec.Code)
	}
}

// With the default list a loopback peer is no trusted proxy either: behind
// an SSH tunnel every client is loopback and names no address of its own.
func TestThrottleIgnoresForwardedByDefault(t *testing.T) {
	e := newEnv(t, fixture(t))
	for i := range auth.FreeFailures {
		r := signInRequest("demo-admin", "demo-wrong-pass-1", "127.0.0.1")
		r.Header.Set("X-Forwarded-For", netip.AddrFrom4([4]byte{203, 0, 113, byte(50 + i)}).String())
		if rec := e.do(r); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, rec.Code)
		}
	}
	r := signInRequest("demo-admin", password(t, "demo-admin"), "127.0.0.1")
	r.Header.Set("X-Forwarded-For", "203.0.113.60")
	if rec := e.do(r); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a new forwarded address from loopback: %d, want 429", rec.Code)
	}
}

// The session cookie is Secure over TLS and behind a trusted proxy that says
// https, and only there (review 6, finding 4).
func TestCookieSecureBehindTrustedProxy(t *testing.T) {
	e := newEnvWith(t, auth.Options{Snapshot: fixture(t), TrustedProxies: proxyList(t, "10.0.0.0/8")})
	signIn := func(peer string, proto ...string) *http.Cookie {
		t.Helper()
		r := signInRequest("demo-admin", password(t, "demo-admin"), peer)
		for _, p := range proto {
			r.Header.Add("X-Forwarded-Proto", p)
		}
		return sessionCookie(t, e.do(r))
	}
	if c := signIn("10.0.0.1", "https"); !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("behind a trusted proxy with https: %+v", c)
	}
	if c := signIn("10.0.0.1", "http"); c.Secure {
		t.Errorf("behind a trusted proxy with http: %+v", c)
	}
	if c := signIn("198.51.100.4", "https"); c.Secure {
		t.Errorf("an untrusted peer that says https: %+v", c)
	}
	// the cookie that clears a dead session carries the same flag
	stale := &http.Cookie{Name: auth.CookieName, Value: "demo-stale-session"}
	r := request(http.MethodGet, "/api/session", stale)
	r.RemoteAddr = "10.0.0.1:40000"
	r.Header.Set("X-Forwarded-Proto", "https")
	if c := sessionCookie(t, e.do(r)); c.MaxAge >= 0 || !c.Secure {
		t.Errorf("the clearing cookie behind a trusted proxy with https: %+v", c)
	}
	plain := newEnv(t, fixture(t))
	r = signInRequest("demo-admin", password(t, "demo-admin"), "127.0.0.1")
	r.Header.Set("X-Forwarded-Proto", "https")
	if c := sessionCookie(t, plain.do(r)); c.Secure {
		t.Errorf("the default list, loopback with https: %+v", c)
	}
}
