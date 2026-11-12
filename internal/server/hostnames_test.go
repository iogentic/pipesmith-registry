package server

import (
	"net"
	"net/http/httptest"
	"testing"
)

func TestHostNamesOpenTheGate(t *testing.T) {
	a := newApp(t, nil, appOptions{noScan: true, hostNames: []string{"registry.test", "10.0.0.5", "[fd00::5]"}})
	_, port, _ := net.SplitHostPort(a.addr())
	for _, host := range []string{"registry.test", "Registry.Test:" + port, "10.0.0.5:" + port, "[fd00::5]:" + port, "localhost:" + port} {
		if r := do(t, a.addr(), "GET", "/api/health", nil, "Host", host); r.status != 200 {
			t.Errorf("Host %s: %d %q", host, r.status, r.body)
		}
	}
	for _, host := range []string{"evil.example", "registry.test.evil.example", "10.0.0.6:" + port, "registry.test:" + port + ":1"} {
		if r := do(t, a.addr(), "GET", "/api/health", nil, "Host", host); r.status != 403 {
			t.Errorf("Host %s: %d %q", host, r.status, r.body)
		}
	}
}

func TestWithoutHostNamesTheGateStaysLocal(t *testing.T) {
	a := newApp(t, nil, appOptions{noScan: true})
	if r := do(t, a.addr(), "GET", "/api/health", nil, "Host", "registry.test"); r.status != 403 {
		t.Fatalf("%d %q", r.status, r.body)
	}
}

func TestScanGuardTakesANamedHost(t *testing.T) {
	req := func(host, origin string) (string, bool) {
		r := httptest.NewRequest("POST", "/api/scan", nil)
		r.Host = host
		r.Header.Set(postHeader, "1")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return postRefusal(r, 8081, []string{"registry.test"})
	}
	for _, c := range []struct {
		host, origin string
		refused      bool
	}{
		{"registry.test:8081", "http://registry.test:8081", false},
		{"registry.test:8081", "", false},
		{"localhost:8081", "http://localhost:8081", false},
		{"registry.test:8082", "", true},
		{"registry.test:8081", "http://evil.example", true},
		{"evil.example:8081", "", true},
	} {
		if why, refused := req(c.host, c.origin); refused != c.refused {
			t.Errorf("Host %s Origin %q: refused %v (%s), want %v", c.host, c.origin, refused, why, c.refused)
		}
	}
	r := httptest.NewRequest("POST", "/api/scan", nil)
	r.Host = "registry.test:8081"
	r.Header.Set(postHeader, "1")
	if _, refused := postRefusal(r, 8081, nil); !refused {
		t.Fatal("a named host passed without host names")
	}
}
