package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The TLS listener of decision E9 of the execution plan: one certificate for
// the main listener and every Docker port, TLS 1.2 or newer, HTTP/1.1 only.

func tlsPair(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	certFile, keyFile, pool, err := testkit.TLSPair(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, pool
}

func TestLoadTLS(t *testing.T) {
	certFile, keyFile, _ := tlsPair(t)
	cfg, err := LoadTLS(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 || len(cfg.Certificates) != 1 || !slices.Equal(cfg.NextProtos, []string{"http/1.1"}) {
		t.Fatalf("config: min %x, %d certificates, protocols %q", cfg.MinVersion, len(cfg.Certificates), cfg.NextProtos)
	}
	missing := filepath.Join(t.TempDir(), "missing-key.pem")
	if _, err := LoadTLS(certFile, missing); err == nil || !strings.HasPrefix(err.Error(), "TLS certificate "+certFile+" with key "+missing+": ") ||
		!strings.Contains(err.Error(), "no such file") {
		t.Fatalf("a missing key: %v", err)
	}
	_, otherKey, _ := tlsPair(t)
	if _, err := LoadTLS(certFile, otherKey); err == nil || !strings.Contains(err.Error(), "private key does not match public key") {
		t.Fatalf("the key of another pair: %v", err)
	}
}

// uvicorn starts the scheme of a request on its TLS listener at https; a
// trusted proxy may still name another one.
func TestForwardedKeepsTLS(t *testing.T) {
	trusted := newTrustedHosts("127.0.0.1")
	for _, c := range []struct {
		peer, proto, want string
	}{
		{"203.0.113.9", "http", "https"},
		{"127.0.0.1", "", "https"},
		{"127.0.0.1", "http", "http"},
	} {
		h := http.Header{}
		if c.proto != "" {
			h.Set("X-Forwarded-Proto", c.proto)
		}
		if scheme, _ := trusted.forwarded("https", c.peer, "50123", h); scheme != c.want {
			t.Errorf("peer %s, proto %q: %q, want %q", c.peer, c.proto, scheme, c.want)
		}
	}
}

func TestServeTLS(t *testing.T) {
	certFile, keyFile, pool := tlsPair(t)
	tlsConfig, err := LoadTLS(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	docker := freePort(t)
	cfg := serveConfig(t, dockerDef("demo-docker", docker))
	cfg.Port = 0
	access := &syncBuffer{}
	ready := make(chan []net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, cfg, ServeOptions{AccessLog: access, TLS: tlsConfig, Ready: func(addrs []net.Addr) { ready <- addrs }})
	}()
	var addrs []net.Addr
	select {
	case addrs = <-ready:
	case err := <-done:
		t.Fatalf("serve ended: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	mainAddr, dockerAddr := addrs[0].String(), addrs[1].String()
	client := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
	get := func(url string) *http.Response {
		t.Helper()
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	if resp := get("https://" + mainAddr + "/api/health"); resp.StatusCode != 200 || resp.TLS == nil ||
		resp.TLS.Version < tls.VersionTLS12 || resp.Proto != "HTTP/1.1" {
		t.Fatalf("main port: %d %v %s", resp.StatusCode, resp.TLS, resp.Proto)
	}
	if resp := get("https://" + dockerAddr + "/v2/"); resp.StatusCode != 200 || resp.TLS == nil {
		t.Fatalf("Docker port: %d %v", resp.StatusCode, resp.TLS)
	}
	// the request knows it came over TLS: the redirect of a trailing slash
	// names https
	if resp := get("https://" + mainAddr + "/api/health/"); resp.StatusCode != http.StatusTemporaryRedirect ||
		resp.Header.Get("Location") != "https://"+mainAddr+"/api/health" {
		t.Fatalf("redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// plain HTTP on a TLS port gets no answer
	plain := &http.Client{Timeout: 10 * time.Second}
	if resp, err := plain.Get("http://" + mainAddr + "/api/health"); err == nil {
		resp.Body.Close()
		t.Fatalf("plain HTTP on the TLS port: %d", resp.StatusCode)
	}
	// TLS 1.1 is refused, TLS 1.2 is taken
	for _, addr := range []string{mainAddr, dockerAddr} {
		if conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}); err == nil {
			conn.Close()
			t.Fatalf("%s: a TLS 1.1 handshake passed", addr)
		}
		conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatalf("%s: TLS 1.2: %v", addr, err)
		}
		if v := conn.ConnectionState().Version; v != tls.VersionTLS12 {
			t.Fatalf("%s: version %x", addr, v)
		}
		conn.Close()
	}
	if !strings.Contains(access.String(), `"GET /api/health HTTP/1.1" 200 OK`) {
		t.Fatalf("access %q", access.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("serve did not stop")
	}
}
