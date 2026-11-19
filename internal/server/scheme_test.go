package server

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// Review S3: one function gives the scheme of a request. From a peer of
// trusted_proxies whose X-Forwarded-Proto names https, the realm of the
// Bearer challenge and the tarball URLs of npm name https, also when the
// peer is not in FORWARDED_ALLOW_IPS; without trusted_proxies they stay as
// uvicorn builds them.
func TestSchemeBehindATrustedProxy(t *testing.T) {
	t.Setenv("FORWARDED_ALLOW_IPS", "192.0.2.1")
	trusted := auth.TrustedProxies{netip.MustParsePrefix("127.0.0.1/32")}
	for _, c := range []struct {
		name   string
		secure func(*http.Request) bool
		want   string
	}{{"no trusted proxy", nil, "http"}, {"loopback a trusted proxy", trusted.Secure, "https"}} {
		t.Run(c.name, func(t *testing.T) {
			with := func(o *appOptions) { o.secure = c.secure }
			a := newTokenApp(t, tokenSnapshot(t), with)
			_, port, _ := strings.Cut(a.docker["demo-docker"].Listener.Addr().String(), ":")
			host := "localhost:" + port
			got := challenge(t, a.on("demo-docker", "GET", "/v2/", "Host", host, "X-Forwarded-Proto", "http, https"))
			if want := `Bearer realm="` + c.want + "://" + host + `/v2/token",service="Pipesmith Registry"`; got != want {
				t.Errorf("challenge %q, want %q", got, want)
			}
			npm := npmProxyApp(t, with)
			r := npm.get("/repository/demo-npm/demo-lib", "Host", "localhost:14193", "X-Forwarded-Proto", "https")
			expect(t, r, 200)
			if want := `"tarball": "` + c.want + `://localhost:14193/repository/demo-npm/`; !strings.Contains(string(r.body), want) {
				t.Errorf("tarball URL, want %s: %s", want, r.body)
			}
		})
	}
}

// Review R3 of the second security review: the REST answers take the scheme
// from the same function as every other URL the server builds, so behind a
// TLS proxy of trusted_proxies, or from a peer of FORWARDED_ALLOW_IPS, the
// url of a repository and the downloadUrl of an asset name https.
func TestRESTURLsTakeTheSchemeOfTheRequest(t *testing.T) {
	trusted := auth.TrustedProxies{netip.MustParsePrefix("127.0.0.1/32")}
	for _, c := range []struct {
		name, allow string
		secure      func(*http.Request) bool
		want        string
	}{
		{"no trusted proxy", "192.0.2.1", nil, "http"},
		{"loopback a trusted proxy", "192.0.2.1", trusted.Secure, "https"},
		{"loopback in FORWARDED_ALLOW_IPS", "127.0.0.1", nil, "https"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FORWARDED_ALLOW_IPS", c.allow)
			a := newTokenApp(t, tokenSnapshot(t), func(o *appOptions) { o.secure = c.secure })
			base := c.want + "://localhost:14193/repository/"
			as := []string{"Host", "localhost:14193", "X-Forwarded-Proto", "https", "Authorization", basicPair(adminUser, adminPass)}
			list := do(t, a.addr(), "GET", "/service/rest/v1/repositories", nil, as...)
			status(t, list, 200)
			rows, _ := list.json(t).([]any)
			if len(rows) == 0 {
				t.Fatalf("no repositories: %q", list.body)
			}
			for _, row := range rows {
				m, _ := row.(map[string]any)
				if m["url"] != base+m["name"].(string) {
					t.Errorf("url %v, want %s%v", m["url"], base, m["name"])
				}
			}
			one := do(t, a.addr(), "GET", "/service/rest/v1/repositories/demo-maven", nil, as...)
			if status(t, one, 200); one.object(t)["url"] != base+"demo-maven" {
				t.Errorf("url of one repository: %q", one.body)
			}
			asset := do(t, a.addr(), "GET", "/service/rest/v1/assets/"+rest.AssetID("demo-maven", jarPath), nil, as...)
			if status(t, asset, 200); asset.object(t)["downloadUrl"] != base+"demo-maven/"+jarPath {
				t.Errorf("downloadUrl: %q", asset.body)
			}
		})
	}
}
