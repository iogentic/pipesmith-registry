package server

import (
	"crypto/sha1" // #nosec G505 -- the dist.shasum of an npm version is a SHA-1, not a security use.
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Task 12 of the phase 5 plan, npm. A package document is fetched from the
// remote, stored through ingest and answered by the read path of stored
// documents, with tarball addresses of this server (decision 5.11: the
// address a document names is never asked). The metadata age applies to
// documents, the content age to tarballs. A scoped name reaches the remote
// as one escaped segment. A tarball must have the integrity or the SHA-1 the
// cached document gives it (decision 5.10). The audit and search calls stay
// with this server (decision 5.18). With upstream on, a proxy that reaches
// its remote answers every version of its document, also without a stored
// tarball (decision 5.19).

const npmProxy = "demo-npm-proxy"

// npmVersion is one version of a document of the remote: its tarball bytes
// and the address its dist.tarball names.
type npmVersion struct {
	version, tarball string
	data             []byte
	integrity        string // empty is the sha512 of data
}

func integrityOf(data []byte) string {
	sum := sha512.Sum512(data)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func shasumOf(data []byte) string {
	sum := sha1.Sum(data) // #nosec G401 -- see the import.
	return hex.EncodeToString(sum[:])
}

// npmDocument is a package document as a registry serves it.
func npmDocument(t *testing.T, name string, versions ...npmVersion) []byte {
	t.Helper()
	entries := map[string]any{}
	times := map[string]any{}
	var names []string
	for _, v := range versions {
		integrity := v.integrity
		if integrity == "" {
			integrity = integrityOf(v.data)
		}
		entries[v.version] = map[string]any{"name": name, "version": v.version,
			"dist": map[string]any{"tarball": v.tarball, "shasum": shasumOf(v.data), "integrity": integrity}}
		times[v.version] = "2026-11-19T06:00:00.000Z"
		names = append(names, v.version)
	}
	slices.Sort(names)
	document := map[string]any{"name": name, "dist-tags": map[string]any{"latest": names[len(names)-1]}, "versions": entries,
		"time": times}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// npmVersionsOf is a document answer: its version keys and the dist.tarball of
// each version.
func npmVersionsOf(t *testing.T, r *answer) (versions []string, tarballs map[string]string) {
	t.Helper()
	var document struct {
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(r.body, &document); err != nil {
		t.Fatalf("%v: %d %q", err, r.status, r.body)
	}
	tarballs = map[string]string{}
	for version, entry := range document.Versions {
		versions = append(versions, version)
		tarballs[version] = entry.Dist.Tarball
	}
	slices.Sort(versions)
	return versions, tarballs
}

// A document is fetched once within the metadata age and answered with the
// tarball addresses of this server and every version; a tarball comes from
// the remote address plus the request path, never from the host its
// document names, and is stored once.
func TestProxyNpmFetchesADocumentAndItsTarballs(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: npmProxy, format: "npm"}}})
	elsewhere := testkit.NewRemote(t)
	base := remoteBase(npmProxy)
	one, two := tarballOf(t, "widget", "1.0.0"), tarballOf(t, "widget", "1.1.0")
	document := npmDocument(t, "widget",
		npmVersion{version: "1.0.0", tarball: elsewhere.URL() + "/far/widget/-/widget-1.0.0.tgz", data: one},
		npmVersion{version: "1.1.0", tarball: u.remote.URL() + base + "widget/-/widget-1.1.0.tgz", data: two})
	u.remote.Script(base+"widget", testkit.OK(string(document)).With("Content-Type", "application/json"))
	u.remote.Script(base+"widget/-/widget-1.0.0.tgz", testkit.OK(string(one)))
	u.remote.Script(base+"widget/-/widget-1.1.0.tgz", testkit.OK(string(two)))
	elsewhere.Script("/far/widget/-/widget-1.0.0.tgz", testkit.OK("bytes of another host"))

	r := u.get(npmProxy, "widget")
	if r.status != 200 || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Fatalf("document: %d %v %q", r.status, r.header, r.body)
	}
	versions, tarballs := npmVersionsOf(t, r)
	if !slices.Equal(versions, []string{"1.0.0", "1.1.0"}) {
		t.Fatalf("versions %v: with upstream on every version of the proxy stays (decision 5.19)", versions)
	}
	for version, address := range tarballs {
		want := "http://" + u.addr() + "/repository/" + npmProxy + "/widget/-/widget-" + version + ".tgz"
		if address != want {
			t.Errorf("%s: tarball %s, want %s", version, address, want)
		}
	}
	if row := u.row(npmProxy, "widget"); row == nil || row.Store != "local-write" {
		t.Fatalf("the document is not stored: %+v", row)
	}
	if version := u.get(npmProxy, "widget/1.1.0"); version.status != 200 || !strings.Contains(string(version.body), `"version":"1.1.0"`) &&
		!strings.Contains(string(version.body), `"version": "1.1.0"`) {
		t.Fatalf("version document: %d %q", version.status, version.body)
	}
	for _, c := range []struct {
		version string
		data    []byte
	}{{"1.0.0", one}, {"1.1.0", two}} {
		for i := 0; i < 2; i++ {
			got := u.get(npmProxy, "widget/-/widget-"+c.version+".tgz")
			if got.status != 200 || string(got.body) != string(c.data) {
				t.Fatalf("tarball %s, request %d: %d %q", c.version, i+1, got.status, got.body)
			}
		}
		if n := u.remote.Count(base + "widget/-/widget-" + c.version + ".tgz"); n != 1 {
			t.Errorf("tarball %s: %d requests at the remote, want 1", c.version, n)
		}
	}
	if n := u.remote.Count(base + "widget"); n != 1 {
		t.Errorf("%d requests of the document within its metadata age, want 1", n)
	}
	if n := len(elsewhere.Requests()); n != 0 {
		t.Fatalf("%d requests at the host the document names (decision 5.11)", n)
	}
}

// A tarball whose bytes differ from the integrity or the SHA-1 of the cached
// document answers 502 and stores nothing; a tarball the document does not
// name answers 404 and is not asked for; a tarball whose document is not
// cached fetches the document first.
func TestProxyNpmChecksATarballAgainstItsDocument(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: npmProxy, format: "npm"}}})
	base := remoteBase(npmProxy)
	good, bad, shaOnly := tarballOf(t, "gizmo", "1.0.0"), tarballOf(t, "gizmo", "2.0.0"), tarballOf(t, "gizmo", "3.0.0")
	document := npmDocument(t, "gizmo",
		npmVersion{version: "1.0.0", tarball: "https://registry.example.test/gizmo/-/gizmo-1.0.0.tgz", data: good},
		npmVersion{version: "2.0.0", tarball: "https://registry.example.test/gizmo/-/gizmo-2.0.0.tgz", data: bad},
		npmVersion{version: "3.0.0", tarball: "https://registry.example.test/gizmo/-/gizmo-3.0.0.tgz", data: shaOnly,
			integrity: "sha1-" + base64.StdEncoding.EncodeToString(mustHex(t, shasumOf(shaOnly)))})
	u.remote.Script(base+"gizmo", testkit.OK(string(document)))
	u.remote.Script(base+"gizmo/-/gizmo-1.0.0.tgz", testkit.OK(string(good)))
	u.remote.Script(base+"gizmo/-/gizmo-2.0.0.tgz", testkit.OK("other bytes than the document names"))
	u.remote.Script(base+"gizmo/-/gizmo-3.0.0.tgz", testkit.OK(string(shaOnly)))
	u.remote.Script(base+"gizmo/-/gizmo-9.0.0.tgz", testkit.OK("a tarball of no version"))

	// the document is not cached yet: the tarball asks for it first
	if r := u.get(npmProxy, "gizmo/-/gizmo-1.0.0.tgz"); r.status != 200 || string(r.body) != string(good) {
		t.Fatalf("a tarball before its document: %d %q", r.status, r.body)
	}
	targets := func() []string {
		var out []string
		for _, req := range u.remote.Requests() {
			out = append(out, req.Target)
		}
		return out
	}
	if want := []string{base + "gizmo", base + "gizmo/-/gizmo-1.0.0.tgz"}; !slices.Equal(targets(), want) {
		t.Fatalf("requests %v, want %v", targets(), want)
	}
	if r := u.get(npmProxy, "gizmo/-/gizmo-2.0.0.tgz"); r.status != http.StatusBadGateway {
		t.Fatalf("a tarball with other bytes: %d %q", r.status, r.body)
	}
	if row := u.row(npmProxy, "gizmo/-/gizmo-2.0.0.tgz"); row != nil {
		t.Fatalf("a refused tarball is stored: %+v", row)
	}
	if r := u.get(npmProxy, "gizmo/-/gizmo-3.0.0.tgz"); r.status != 200 || string(r.body) != string(shaOnly) {
		t.Fatalf("a tarball with a sha1 integrity: %d %q", r.status, r.body)
	}
	if r := u.get(npmProxy, "gizmo/-/gizmo-9.0.0.tgz"); r.status != 404 {
		t.Fatalf("a tarball the document does not name: %d %q", r.status, r.body)
	}
	if n := u.remote.Count(base + "gizmo/-/gizmo-9.0.0.tgz"); n != 0 {
		t.Fatalf("%d requests of a tarball the document does not name", n)
	}
	// 2 documents' worth of blobs: the document and two tarballs, nothing of the refused one
	if blobs, work := u.blobFiles(); len(blobs) != 6 || len(work) != 0 {
		t.Fatalf("blobs %v, work files %v", blobs, work)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A scoped name reaches the remote as one segment, @scope%2Fname, whether the
// client sends the slash encoded or not; its tarball is asked under the path
// of the request.
func TestProxyNpmAsksAScopedNameAsOneSegment(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: npmProxy, format: "npm"}}})
	base := remoteBase(npmProxy)
	data := tarballOf(t, "@demo/gadget", "1.0.0")
	document := npmDocument(t, "@demo/gadget",
		npmVersion{version: "1.0.0", tarball: "https://registry.example.test/@demo/gadget/-/gadget-1.0.0.tgz", data: data})
	u.remote.Script(base+"@demo%2Fgadget", testkit.OK(string(document)))
	u.remote.Script(base+"@demo/gadget/-/gadget-1.0.0.tgz", testkit.OK(string(data)))
	for _, target := range []string{"@demo%2fgadget", "@demo/gadget"} {
		r := u.get(npmProxy, target)
		if r.status != 200 {
			t.Fatalf("%s: %d %q", target, r.status, r.body)
		}
		_, tarballs := npmVersionsOf(t, r)
		if want := "http://" + u.addr() + "/repository/" + npmProxy + "/@demo/gadget/-/gadget-1.0.0.tgz"; tarballs["1.0.0"] != want {
			t.Fatalf("%s: tarball %s, want %s", target, tarballs["1.0.0"], want)
		}
	}
	if r := u.get(npmProxy, "@demo/gadget/-/gadget-1.0.0.tgz"); r.status != 200 || string(r.body) != string(data) {
		t.Fatalf("scoped tarball: %d %q", r.status, r.body)
	}
	var targets []string
	for _, req := range u.remote.Requests() {
		targets = append(targets, req.Target)
	}
	if want := []string{base + "@demo%2Fgadget", base + "@demo/gadget/-/gadget-1.0.0.tgz"}; !slices.Equal(targets, want) {
		t.Fatalf("requests %v, want %v", targets, want)
	}
}

// The calls below "-" stay with this server: ping answers here, audit and
// search answer the npm 404 of the port, and none reaches the remote
// (decision 5.18).
func TestProxyNpmPassesNoAuditOrSearch(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: npmProxy, format: "npm"}}})
	for _, c := range []struct{ method, path string }{
		{"GET", "-/ping"},
		{"GET", "-/v1/search?text=widget&size=20"},
		{"GET", "-/npm/v1/security/advisories/bulk"},
		{"POST", "-/npm/v1/security/audits/quick"},
		{"POST", "-/npm/v1/security/advisories/bulk"},
	} {
		r := do(t, u.addr(), c.method, "/repository/"+npmProxy+"/"+c.path, []byte(`{"widget":["1.0.0"]}`), "basic", asAdmin)
		want := 404
		if c.path == "-/ping" {
			want = 200
		}
		if r.status != want {
			t.Errorf("%s %s: %d %q, want %d", c.method, c.path, r.status, r.body, want)
		}
	}
	if n := len(u.remote.Requests()); n != 0 {
		t.Fatalf("%d requests at the remote: %+v", n, u.remote.Requests())
	}
}

// The metadata age applies to a document and the content age to a tarball:
// with metadata age 0 every request of the document asks the remote, with
// content age -1 a tarball is asked once.
func TestProxyNpmAgesOfDocumentsAndTarballs(t *testing.T) {
	state := openProxyState(t)
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: npmProxy, format: "npm",
		attributes: map[string]any{"proxy": map[string]any{"metadataMaxAge": 0, "contentMaxAge": -1}}}},
		tune: func(o *appOptions) { o.proxyState = state }})
	base := remoteBase(npmProxy)
	data := tarballOf(t, "widget", "1.0.0")
	document := npmDocument(t, "widget", npmVersion{version: "1.0.0", tarball: "https://registry.example.test/widget/-/widget-1.0.0.tgz", data: data})
	u.remote.Script(base+"widget", testkit.OK(string(document)).With("ETag", `"doc-1"`), testkit.NotModified())
	u.remote.Script(base+"widget/-/widget-1.0.0.tgz", testkit.OK(string(data)))
	for i := 0; i < 3; i++ {
		if r := u.get(npmProxy, "widget"); r.status != 200 {
			t.Fatalf("document, request %d: %d %q", i+1, r.status, r.body)
		}
		if r := u.get(npmProxy, "widget/-/widget-1.0.0.tgz"); r.status != 200 || string(r.body) != string(data) {
			t.Fatalf("tarball, request %d: %d %q", i+1, r.status, r.body)
		}
	}
	if n := u.remote.Count(base + "widget"); n != 3 {
		t.Errorf("%d requests of the document with metadata age 0, want 3", n)
	}
	if n := u.remote.Count(base + "widget/-/widget-1.0.0.tgz"); n != 1 {
		t.Errorf("%d requests of the tarball with content age -1, want 1", n)
	}
	for _, req := range u.remote.Requests()[1:] {
		if req.Target == base+"widget" && req.Header.Get("If-None-Match") != `"doc-1"` {
			t.Errorf("a check of the stale document without its entity tag: %+v", req)
		}
	}
}

// Decision 5.19 at a proxy, in both positions of the switch: a cached
// document names two versions and the cache holds the tarball of one. With
// upstream off the rule of the port holds and the answer has that one
// version; with upstream on the proxy reaches its remote and both stay.
func TestProxyNpmVersionsWithoutATarball(t *testing.T) {
	data := tarballOf(t, "widget", "1.0.0")
	document := npmDocument(t, "widget",
		npmVersion{version: "1.0.0", tarball: "https://registry.example.test/widget/-/widget-1.0.0.tgz", data: data},
		npmVersion{version: "2.0.0", tarball: "https://registry.example.test/widget/-/widget-2.0.0.tgz", data: []byte("not stored")})
	cached := []testkit.BlobSpec{spec(npmProxy, "widget", document, "application/json"),
		spec(npmProxy, "widget/-/widget-1.0.0.tgz", data, "application/x-tgz")}
	for _, c := range []struct {
		upstream bool
		want     []string
	}{{false, []string{"1.0.0"}}, {true, []string{"1.0.0", "2.0.0"}}} {
		u := startUpstream(t, upstreamSetup{upstream: c.upstream, proxies: []proxyDef{{name: npmProxy, format: "npm"}}, blobs: cached})
		u.remote.Script(remoteBase(npmProxy)+"widget", testkit.NotModified())
		r := u.get(npmProxy, "widget")
		if r.status != 200 {
			t.Fatalf("upstream %v: %d %q", c.upstream, r.status, r.body)
		}
		if versions, _ := npmVersionsOf(t, r); !slices.Equal(versions, c.want) {
			t.Errorf("upstream %v: versions %v, want %v", c.upstream, versions, c.want)
		}
		if n := len(u.remote.Requests()); c.upstream && n != 1 || !c.upstream && n != 0 {
			t.Errorf("upstream %v: %d requests at the remote: %+v", c.upstream, n, u.remote.Requests())
		}
	}
}

// A blocked path of an npm proxy answers 403 with the npm error document
// before the cache is looked at, with upstream on and off, and causes no
// request (decision 5.15, ruling P5-4).
func TestProxyNpmRoutingRule(t *testing.T) {
	data := tarballOf(t, "@demo-blocked/pkg", "1.0.0")
	cached := []testkit.BlobSpec{spec(npmProxy, "@demo-blocked/pkg/-/pkg-1.0.0.tgz", data, "application/x-tgz")}
	for _, upstream := range []bool{false, true} {
		u := startUpstream(t, upstreamSetup{upstream: upstream, blobs: cached, proxies: []proxyDef{{name: npmProxy, format: "npm"}},
			tune: func(o *appOptions) { o.routing = routingOf(npmProxy, "^/@demo-blocked/.*") }})
		u.remote.Script(remoteBase(npmProxy)+"@demo-blocked%2Fpkg", testkit.OK("a document the rule keeps out"))
		for _, path := range []string{"@demo-blocked/pkg", "@demo-blocked%2fpkg", "@demo-blocked/pkg/1.0.0", "@demo-blocked/pkg/-/pkg-1.0.0.tgz"} {
			if r := u.get(npmProxy, path); r.status != http.StatusForbidden || !strings.Contains(string(r.body), `"error"`) {
				t.Errorf("upstream %v, %s: %d %q", upstream, path, r.status, r.body)
			}
		}
		if n := len(u.remote.Requests()); n != 0 {
			t.Errorf("upstream %v: %d requests at the remote for blocked paths", upstream, n)
		}
	}
}
