package server

import (
	"strings"
	"testing"
)

// Review 3-63, point a: the URLs an answer builds from the Host header, the
// realm of the Bearer challenge, the tarball URLs of npm and the URLs of the
// REST answers, take the host name in lower case and a port only when it is
// a port number from 1 to 65535, written without leading zeros. A Host with
// another port gives the Basic challenge on a Docker route and URLs without
// a port elsewhere.
func TestURLsFromTheHostHeaderAreCanonical(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	_, port, _ := strings.Cut(a.docker["demo-docker"].Listener.Addr().String(), ":")
	for _, c := range []struct{ host, want string }{
		{"LocalHost:" + port, `Bearer realm="http://localhost:` + port + `/v2/token"`},
		{"localhost:99999", "Basic "},
		{"localhost:0", "Basic "},
	} {
		if got := challenge(t, a.on("demo-docker", "GET", "/v2/", "Host", c.host)); !strings.HasPrefix(got, c.want) {
			t.Errorf("Host %q: challenge %q, want %q at its start", c.host, got, c.want)
		}
	}

	npm := npmProxyApp(t)
	for _, c := range []struct{ host, want string }{
		{"LOCALHOST:14193", "http://localhost:14193/repository/demo-npm/"},
		{"localhost:99999", "http://localhost/repository/demo-npm/"},
	} {
		r := npm.get("/repository/demo-npm/demo-lib", "Host", c.host)
		expect(t, r, 200)
		if want := `"tarball": "` + c.want; !strings.Contains(string(r.body), want) {
			t.Errorf("Host %q: tarball URL, want %s: %s", c.host, want, r.body)
		}
	}

	for _, c := range []struct{ host, want string }{
		{"LOCALHOST:14193", "http://localhost:14193/repository/demo-maven"},
		{"localhost:99999", "http://localhost/repository/demo-maven"},
	} {
		one := do(t, a.addr(), "GET", "/service/rest/v1/repositories/demo-maven", nil, "Host", c.host,
			"Authorization", basicPair(adminUser, adminPass))
		if status(t, one, 200); one.object(t)["url"] != c.want {
			t.Errorf("Host %q: url of a repository %q, want %s", c.host, one.object(t)["url"], c.want)
		}
	}
}
