package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

type proxyCase struct {
	Trusted    string      `json:"trusted"`
	Peer       string      `json:"peer"`
	Headers    [][2]string `json:"headers"`
	Scheme     string      `json:"scheme"`
	ClientHost string      `json:"client_host"`
	ClientPort string      `json:"client_port"`
	Access     string      `json:"access"`
}

// TestForwardedVectors runs the cases of uvicorn's ProxyHeadersMiddleware:
// the scheme and the client of the access line for a peer, the trusted
// hosts and the headers of a request.
func TestForwardedVectors(t *testing.T) {
	var v struct {
		Default string      `json:"default"`
		Cases   []proxyCase `json:"cases"`
	}
	if err := json.Unmarshal(mustRead(t, "../../testdata/vectors/server_proxy.json"), &v); err != nil {
		t.Fatal(err)
	}
	if v.Default != forwardedDefault {
		t.Fatalf("default trusted hosts %q, Python %q", forwardedDefault, v.Default)
	}
	if len(v.Cases) < 500 {
		t.Fatalf("only %d cases", len(v.Cases))
	}
	forwarded := map[string]int{}
	for _, c := range v.Cases {
		h := http.Header{}
		for _, kv := range c.Headers {
			value, err := base64.StdEncoding.DecodeString(kv[1])
			if err != nil {
				t.Fatal(err)
			}
			h.Add(kv[0], string(value))
		}
		scheme, client := newTrustedHosts(c.Trusted).forwarded(c.Peer, "50123", h)
		if scheme != c.Scheme || client != c.Access {
			t.Errorf("trusted %q, peer %s, headers %q: scheme %q client %q, want %q %q", c.Trusted, c.Peer, c.Headers,
				scheme, client, c.Scheme, c.Access)
		}
		if c.Scheme != "http" {
			forwarded["scheme"]++
		}
		if c.Access != c.Peer+":50123" {
			forwarded["client"]++
		}
	}
	if forwarded["scheme"] < 20 || forwarded["client"] < 100 {
		t.Fatalf("too few cases change the request: %v", forwarded)
	}
}

func npmProxyApp(t *testing.T) *app {
	t.Helper()
	base := t.TempDir()
	document := []byte(`{"name": "demo-lib", "versions": {"1.0.0": {"name": "demo-lib", "version": "1.0.0"}}}`)
	blobs := []testkit.BlobSpec{
		spec("demo-npm", "demo-lib", document, "application/json"),
		spec("demo-npm", "demo-lib/-/demo-lib-1.0.0.tgz", tarballOf(t, "demo-lib", "1.0.0"), ""),
	}
	return newApp(t, []config.StoreConfig{blobStore(t, base, "s1", blobs...)},
		appOptions{defs: []*pyjson.Object{def("demo-npm", "npm", "hosted")}})
}

// TestForwardedProtoSetsTheScheme: from 127.0.0.1 the Python server takes
// X-Forwarded-Proto as the scheme of the tarball URLs, the npm registry of
// the asset record and the Location of a slash redirect.
func TestForwardedProtoSetsTheScheme(t *testing.T) {
	a := npmProxyApp(t)
	host := "localhost:14193"
	r := a.get("/repository/demo-npm/demo-lib", "Host", host, "X-Forwarded-Proto", "https")
	expect(t, r, 200)
	if !strings.Contains(string(r.body), `"tarball": "https://`+host+`/repository/demo-npm/demo-lib/-/demo-lib-1.0.0.tgz"`) {
		t.Fatalf("tarball URL: %s", r.body)
	}
	plain := a.get("/repository/demo-npm/demo-lib", "Host", host)
	if !strings.Contains(string(plain.body), `"tarball": "http://`+host+`/`) || plain.header.Get("ETag") == r.header.Get("ETag") ||
		len(plain.body)+1 != len(r.body) {
		t.Fatalf("plain %s\nforwarded %s", plain, r)
	}
	r = a.get("/repository/demo-npm/demo-lib", "Host", host, "X-Forwarded-Proto", "http", "X-Forwarded-Proto", "wss")
	if !strings.Contains(string(r.body), `"tarball": "wss://`+host+`/`) {
		t.Fatalf("the last X-Forwarded-Proto counts: %s", r.body)
	}
	r = a.get("/repository/demo-npm/demo-lib", "Host", host, "X-Forwarded-Proto", "ftp")
	if !strings.Contains(string(r.body), `"tarball": "http://`+host+`/`) {
		t.Fatalf("an unknown scheme is ignored: %s", r.body)
	}
	r = a.get("/api/asset/demo-npm?path=demo-lib", "Host", host, "X-Forwarded-Proto", "https")
	expect(t, r, 200)
	if !strings.Contains(string(r.body), `"registry":"https://`+host+`/repository/demo-npm/"`) {
		t.Fatalf("asset record: %s", r.body)
	}
	r = a.get("/api/health/", "Host", host, "X-Forwarded-Proto", "https")
	expect(t, r, 307)
	if r.header.Get("Location") != "https://"+host+"/api/health" {
		t.Fatalf("redirect: %s", r)
	}
}

// TestForwardedForSetsTheClient: the access line names the client of
// X-Forwarded-For with port 0, as uvicorn logs it.
func TestForwardedForSetsTheClient(t *testing.T) {
	a := npmProxyApp(t)
	a.get("/api/health/", "X-Forwarded-For", "203.0.113.9")
	a.get("/api/health", "X-Forwarded-For", "203.0.113.9:8080, 127.0.0.1")
	a.get("/api/health", "X-Forwarded-For", "[2001:db8::1]:443")
	a.get("/api/health", "X-Forwarded-For", "")
	a.get("/api/health", "Host", "example.com", "X-Forwarded-For", "198.51.100.7")
	lines := strings.Split(strings.TrimSpace(a.access.String()), "\n")
	want := []string{
		`INFO:     203.0.113.9:0 - "GET /api/health/ HTTP/1.1" 307 Temporary Redirect`,
		`INFO:     203.0.113.9:8080 - "GET /api/health HTTP/1.1" 200 OK`,
		`INFO:     2001:db8::1:443 - "GET /api/health HTTP/1.1" 200 OK`,
		``,
		`INFO:     198.51.100.7:0 - "GET /api/health HTTP/1.1" 403 Forbidden`,
	}
	if len(lines) != len(want) {
		t.Fatalf("access lines %q", lines)
	}
	peer := regexp.MustCompile(`^INFO:     127\.0\.0\.1:\d+ - "GET /api/health HTTP/1\.1" 200 OK$`)
	for i, line := range lines {
		if want[i] == "" {
			if !peer.MatchString(line) {
				t.Errorf("an empty X-Forwarded-For keeps the peer: %q", line)
			}
			continue
		}
		if line != want[i] {
			t.Errorf("access line %q, want %q", line, want[i])
		}
	}
}
