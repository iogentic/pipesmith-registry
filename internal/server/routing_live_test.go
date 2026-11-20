package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

const (
	proxyBlocked = "/repository/demo-raw-proxy/com/example/blocked/x.txt"
	proxyOther   = "/repository/demo-raw-proxy/com/example/other/x.txt"
	groupFile    = "/repository/demo-raw-group/docs/readme.txt"
	hostedFile   = "/repository/demo-raw/docs/readme.txt"
)

// newProxyRoutingApp is the routing fake over the invented stores with a
// raw proxy, linked to demo-block, and a raw group of the hosted raw
// repository and the proxy. Upstream is off, so no remote is asked.
func newProxyRoutingApp(t *testing.T) (*app, *routingSecurity) {
	t.Helper()
	sec := &routingSecurity{fakeSecurity: newFakeSecurity(), rules: model.Routing{
		Rules: []model.RoutingRule{{Name: "demo-block", Mode: "BLOCK", Matchers: []string{"^/com/example/blocked/.*"}}},
		Links: map[string]string{"demo-raw-proxy": "demo-block"}}}
	base := t.TempDir()
	stores := []config.StoreConfig{blobStore(t, base, "store-a",
		spec("demo-raw", "docs/readme.txt", []byte("readme"), "text/plain"))}
	opts := appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted"),
		def("demo-raw-proxy", "raw", "proxy").Set("remote_url", "https://203.0.113.10/raw/"),
		def("demo-raw-group", "raw", "group", "demo-raw", "demo-raw-proxy")}, security: sec}
	return startApp(t, base, prepare(t, base, stores, opts), opts), sec
}

func adminGet(t *testing.T, a *app, target string) *answer {
	t.Helper()
	return do(t, a.addr(), "GET", target, nil, "basic", asAdmin)
}

// routingLines are the log lines of the routing rules that are no problem
// of a rule.
func routingLines(log *records) []string {
	var out []string
	for _, line := range log.all() {
		if strings.Contains(line, "routing rules") {
			out = append(out, line)
		}
	}
	return out
}

// SY9 and SY10: while the generation or the rules cannot be read, a request
// that a routing rule could decide answers 503 and one that no rule decides
// answers as before; after the read works again the block holds. The
// failure is logged once when it starts and once when it ends, without a
// path.
func TestRoutingFailsClosed(t *testing.T) {
	for _, c := range []struct {
		name                string
		generation, routing bool
	}{{"the generation", true, false}, {"the rules", false, true}} {
		t.Run(c.name, func(t *testing.T) {
			log := captureLog(t)
			a, sec := newProxyRoutingApp(t)
			if r := adminGet(t, a, proxyBlocked); r.status != 403 {
				t.Fatalf("the block: %d %s", r.status, r.body)
			}
			status(t, adminGet(t, a, proxyOther), 404)
			status(t, adminGet(t, a, hostedFile), 200)
			sec.fail(c.generation, c.routing)
			if c.routing {
				sec.link("demo-raw-proxy", "demo-block") // a new generation whose rules cannot be read
			}
			for range 3 {
				for _, target := range []string{proxyBlocked, proxyOther, groupFile} {
					r := adminGet(t, a, target)
					if r.status != 503 || !strings.Contains(string(r.body), "routing rules cannot be read") {
						t.Fatalf("%s while the rules cannot be read: %d %s", target, r.status, r.body)
					}
				}
				status(t, adminGet(t, a, hostedFile), 200)
				status(t, adminGet(t, a, "/service/rest/v1/repositorySettings"), 200)
			}
			if lines := routingLines(log); len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN") {
				t.Fatalf("lines while failing: %q", lines)
			}
			sec.fail(false, false)
			status(t, adminGet(t, a, proxyBlocked), 403)
			status(t, adminGet(t, a, proxyOther), 404)
			status(t, adminGet(t, a, groupFile), 200)
			lines := routingLines(log)
			if len(lines) != 2 || !strings.HasPrefix(lines[1], "INFO") {
				t.Fatalf("lines after the read works: %q", lines)
			}
			for _, line := range lines {
				if strings.Contains(line, "/") {
					t.Fatalf("the line names a path: %q", line)
				}
			}
		})
	}
}

// The preview of a proxy names its rule and the verdict for a blocked and
// an allowed path (contract S18).
func TestRoutingPreviewOfAProxy(t *testing.T) {
	a, _ := newProxyRoutingApp(t)
	for path, want := range map[string]bool{"/com/example/blocked/x.txt": false, "/com/example/other/x.txt": true} {
		r := do(t, a.addr(), "POST", previewPath, []byte(`{"scope":"proxy","path":"`+path+`"}`), "basic", asAdmin,
			"Content-Type", jsonType)
		status(t, r, 200)
		var v struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(r.body, &v); err != nil {
			t.Fatal(err)
		}
		if len(v.Items) != 1 || v.Items[0]["repository"] != "demo-raw-proxy" || v.Items[0]["rule"] != "demo-block" ||
			v.Items[0]["allowed"] != want || v.Items[0]["format"] != "raw" || v.Items[0]["type"] != "proxy" {
			t.Fatalf("%s: %s", path, r.body)
		}
	}
	r := do(t, a.addr(), "POST", previewPath, []byte(`{"scope":"group","path":"/com/example/blocked/x.txt"}`), "basic", asAdmin,
		"Content-Type", jsonType)
	if !strings.Contains(string(r.body), `{"repository":"demo-raw-group","type":"group","format":"raw","rule":null,"allowed":true}`) {
		t.Fatalf("the group: %s", r.body)
	}
}
