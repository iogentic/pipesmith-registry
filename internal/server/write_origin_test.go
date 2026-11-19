package server

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// The origin check of a write (assessment A3 of review 4) and the scheme of
// the security stream (review S3) agree behind a reverse proxy of
// trusted_proxies (leg 4h): the origin of a request is the one its client
// sees, scheme and Host, so a page of the interface behind a proxy that
// ends TLS writes, and a page of the plain HTTP origin of the same name does
// not. FORWARDED_ALLOW_IPS names no address of the test, so only the trusted
// proxies of serve decide the scheme.

func startBehindProxy(t *testing.T, proxies auth.TrustedProxies) *writing {
	t.Helper()
	t.Setenv("FORWARDED_ALLOW_IPS", "192.0.2.250")
	sec := newFakeSecurity()
	snap := *sec.snap
	snap.Repositories = []model.Repository{{Name: "demo-raw", Format: "raw", Type: "hosted", Online: true, Attributes: storage("ALLOW")}}
	sec.snap = &snap
	base := t.TempDir()
	storeCfgs := []config.StoreConfig{blobStore(t, base, "store-a")}
	w := &writing{catalog: &fakeCatalog{}, clock: &clock{now: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}, old: storeCfgs[0].Root}
	w.opts = appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}, security: sec, writes: true, catalog: w.catalog,
		writeOptions: blobwrite.Options{Now: w.clock.Now}, hostNames: []string{"registry.example.com"}, secure: proxies.Secure}
	w.app = startApp(t, base, prepare(t, base, storeCfgs, w.opts), w.opts)
	return w
}

func TestTheOriginOfAWriteBehindATrustedProxy(t *testing.T) {
	loopback := auth.TrustedProxies{netip.MustParsePrefix("127.0.0.1/32")}
	for _, c := range []struct {
		name    string
		proxies auth.TrustedProxies
		proto   string // X-Forwarded-Proto of the request
		origin  string
		want    int
	}{
		{"a page of the TLS origin through the proxy", loopback, "https", "https://registry.example.com", 201},
		{"the TLS origin with its default port", loopback, "https", "https://registry.example.com:443", 201},
		{"a page of the plain origin through a TLS proxy", loopback, "https", "http://registry.example.com", 403},
		{"a page of the plain origin through a plain proxy", loopback, "http", "http://registry.example.com", 201},
		{"a page of the TLS origin without the header of the proxy", loopback, "", "https://registry.example.com", 403},
		{"another host", loopback, "https", "https://pages.example.net", 403},
		{"a peer that is no trusted proxy says https", nil, "https", "https://registry.example.com", 403},
		{"a peer that is no trusted proxy, plain origin", nil, "https", "http://registry.example.com", 201},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := startBehindProxy(t, c.proxies)
			headers := []string{"basic", asAdmin, "Host", "registry.example.com", "Origin", c.origin, "Sec-Fetch-Site", "same-origin"}
			if c.proto != "" {
				headers = append(headers, "X-Forwarded-Proto", c.proto)
			}
			put := do(t, w.addr(), "PUT", "/repository/demo-raw/origin.txt", []byte("origin"), headers...)
			if put.status != c.want {
				t.Fatalf("raw PUT: %d %q, want %d", put.status, put.body, c.want)
			}
			if c.want == 403 && !strings.Contains(string(put.body), crossSiteRefusal) {
				t.Fatalf("raw PUT: %q", put.body)
			}
			form, contentType := multipartBody(t, formField("raw.directory", "origin"), formFile("raw.asset1", "a.txt", []byte("a")),
				formField("raw.asset1.filename", "a.txt"))
			upload := do(t, w.addr(), "POST", componentsPath+"?repository=demo-raw", form, append(headers, "Content-Type", contentType)...)
			if want := map[int]int{201: 204, 403: 403}[c.want]; upload.status != want {
				t.Fatalf("REST upload: %d %q, want %d", upload.status, upload.body, want)
			}
		})
	}
}
