package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Task 12 of the phase 5 plan, docker. A manifest by tag is fetched with the
// Accept values of the client, its SHA-256 is computed from the bytes, and
// it is stored under tag and digest as a pushed manifest is. A manifest by
// digest and a blob must hash to the digest of the request (decision 5.10)
// and are never checked again; a manifest by tag and the tag list follow the
// metadata age. HEAD of an uncached manifest answers with the digest and the
// content type of the remote and stores nothing. A blob is stored once
// under the shared blob path and fetched under the name of the request. The
// Docker port of the proxy reads as its repository path does. The token
// flow to the remote (ruling P5-10) is tested in internal/proxy and here once
// through the server.

const dockerProxy = "demo-docker-proxy"

// remoteImage scripts an image at the remote of a proxy: blobs under the name
// of the image, manifests with the content type and the digest header a
// registry sends.
func remoteImage(u *upstreamApp, proxyName, name string, files map[string]file, wrap func(testkit.Answer) testkit.Answer) {
	base := remoteBase(proxyName)
	if wrap == nil {
		wrap = func(a testkit.Answer) testkit.Answer { return a }
	}
	for path, f := range files {
		if digest, ok := strings.CutPrefix(path, dockerBlobPrefix); ok {
			u.remote.Script(base+"v2/"+name+"/blobs/"+digest, wrap(testkit.OK(string(f.data))))
			continue
		}
		u.remote.Script(base+path, wrap(testkit.OK(string(f.data)).With("Content-Type", f.contentType).With("Docker-Content-Digest", digestOf(f.data))))
	}
}

// imageParts are the tag manifest, its digest and the blob digests of an
// image built by dockerImage.
func imageParts(files map[string]file, name, tag string) (manifest []byte, digest string, blobs []string) {
	manifest = files["v2/"+name+"/manifests/"+tag].data
	for path := range files {
		if d, ok := strings.CutPrefix(path, dockerBlobPrefix); ok {
			blobs = append(blobs, d)
		}
	}
	slices.Sort(blobs)
	return manifest, digestOf(manifest), blobs
}

func dockerGet(u *upstreamApp, path string, headers ...string) *answer {
	u.t.Helper()
	return u.get(dockerProxy, path, headers...)
}

func TestProxyDockerPullsThroughTheProxy(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: dockerProxy, format: "docker"}},
		tune: func(o *appOptions) { o.dockerRepos = []string{dockerProxy} }})
	files := dockerImage("demo/app", "1.0", []byte("the layer of demo/app"))
	remoteImage(u, dockerProxy, "demo/app", files, nil)
	manifest, digest, blobs := imageParts(files, "demo/app", "1.0")
	base := remoteBase(dockerProxy)

	r := dockerGet(u, "v2/demo/app/manifests/1.0", "Accept", dockerManifestType+", "+ociIndexType)
	if r.status != 200 || string(r.body) != string(manifest) || r.header.Get("Docker-Content-Digest") != digest ||
		r.header.Get("Content-Type") != dockerManifestType {
		t.Fatalf("manifest by tag: %d %v %q", r.status, r.header, r.body)
	}
	asked := u.remote.Requests()
	if len(asked) != 1 || asked[0].Target != base+"v2/demo/app/manifests/1.0" || asked[0].Header.Get("Accept") != dockerManifestType+", "+ociIndexType {
		t.Fatalf("the manifest request at the remote: %+v", asked)
	}
	// stored under tag and digest, the tag a hard link of the digest blob, as a pushed manifest is
	tag, byDigest := u.row(dockerProxy, "v2/demo/app/manifests/1.0"), u.row(dockerProxy, "v2/demo/app/manifests/"+digest)
	if tag == nil || byDigest == nil || tag.Store != "local-write" || byDigest.Store != "local-write" {
		t.Fatalf("rows: tag %+v, digest %+v", tag, byDigest)
	}
	one, err := os.Stat(filepath.Join(u.root(), tag.Location))
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.Stat(filepath.Join(u.root(), byDigest.Location))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(one, two) {
		t.Fatalf("the tag %s is no hard link of the digest blob %s", tag.Location, byDigest.Location)
	}
	if r := dockerGet(u, "v2/demo/app/manifests/"+digest); r.status != 200 || string(r.body) != string(manifest) {
		t.Fatalf("manifest by digest: %d %q", r.status, r.body)
	}
	for _, blob := range blobs {
		r := dockerGet(u, "v2/demo/app/blobs/"+blob)
		if r.status != 200 || string(r.body) != string(files[dockerBlobPrefix+blob].data) || r.header.Get("Docker-Content-Digest") != blob {
			t.Fatalf("blob %s: %d %v %q", blob, r.status, r.header, r.body)
		}
		if row := u.row(dockerProxy, dockerBlobPrefix+blob); row == nil || row.Store != "local-write" {
			t.Fatalf("blob %s is not stored under the shared blob path: %+v", blob, row)
		}
	}
	// within the metadata age nothing is asked again; digests are never asked again
	before := len(u.remote.Requests())
	dockerGet(u, "v2/demo/app/manifests/1.0")
	dockerGet(u, "v2/demo/app/manifests/"+digest)
	for _, blob := range blobs {
		dockerGet(u, "v2/demo/app/blobs/"+blob)
	}
	if after := len(u.remote.Requests()); after != before || before != 1+len(blobs) {
		t.Fatalf("requests at the remote: %d after the first pass, %d after the second: %+v", before, after, u.remote.Requests())
	}
	// the Docker port of the proxy reads as its repository path
	port := u.on(dockerProxy, "GET", "/v2/demo/app/manifests/1.0", "Authorization", basicOf(asAdmin))
	if port.status != 200 || string(port.body) != string(manifest) {
		t.Fatalf("manifest on the Docker port: %d %q", port.status, port.body)
	}
	if after := len(u.remote.Requests()); after != before {
		t.Fatalf("the Docker port asked the remote again: %+v", u.remote.Requests())
	}
}

// basicOf is the Authorization value of Basic credentials "user:password".
func basicOf(pair string) string {
	r, _ := http.NewRequest("GET", "http://localhost/", nil)
	user, password, _ := strings.Cut(pair, ":")
	r.SetBasicAuth(user, password)
	return r.Header.Get("Authorization")
}

// A blob or a manifest by digest whose bytes have another digest, and a
// manifest by tag whose digest header differs from its bytes, answer 502
// and store nothing.
func TestProxyDockerChecksDigests(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: dockerProxy, format: "docker"}}})
	base := remoteBase(dockerProxy)
	named := digestOf([]byte("the bytes the digest names"))
	u.remote.Script(base+"v2/demo/app/blobs/"+named, testkit.OK("other bytes"))
	u.remote.Script(base+"v2/demo/app/manifests/"+named, testkit.OK(`{"schemaVersion": 2}`).With("Content-Type", dockerManifestType))
	u.remote.Script(base+"v2/demo/app/manifests/2.0", testkit.OK(`{"schemaVersion": 2, "tag": "2.0"}`).With("Content-Type", dockerManifestType).
		With("Docker-Content-Digest", named))
	for _, path := range []string{"v2/demo/app/blobs/" + named, "v2/demo/app/manifests/" + named, "v2/demo/app/manifests/2.0"} {
		if r := dockerGet(u, path); r.status != http.StatusBadGateway {
			t.Errorf("%s: %d %q", path, r.status, r.body)
		}
	}
	if blobs, work := u.blobFiles(); len(blobs) != 0 || len(work) != 0 {
		t.Fatalf("blobs %v, work files %v", blobs, work)
	}
}

// HEAD of an uncached manifest answers with the digest and the content type
// of the remote and stores nothing; the remote sees HEAD.
func TestProxyDockerHeadOfAnUncachedManifest(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: dockerProxy, format: "docker"}}})
	files := dockerImage("demo/app", "1.0", []byte("a layer"))
	remoteImage(u, dockerProxy, "demo/app", files, nil)
	manifest, digest, _ := imageParts(files, "demo/app", "1.0")
	r := do(t, u.addr(), "HEAD", "/repository/"+dockerProxy+"/v2/demo/app/manifests/1.0", nil, "basic", asAdmin, "Accept", dockerManifestType)
	if r.status != 200 || len(r.body) != 0 || r.header.Get("Docker-Content-Digest") != digest || r.header.Get("Content-Type") != dockerManifestType {
		t.Fatalf("HEAD: %d %v %q", r.status, r.header, r.body)
	}
	asked := u.remote.Requests()
	if len(asked) != 1 || asked[0].Method != "HEAD" || asked[0].Header.Get("Accept") != dockerManifestType {
		t.Fatalf("requests at the remote: %+v", asked)
	}
	if blobs, _ := u.blobFiles(); len(blobs) != 0 || u.row(dockerProxy, "v2/demo/app/manifests/1.0") != nil {
		t.Fatalf("HEAD stored %v", blobs)
	}
	if r := do(t, u.addr(), "HEAD", "/repository/"+dockerProxy+"/v2/demo/app/manifests/9.9", nil, "basic", asAdmin); r.status != 404 {
		t.Fatalf("HEAD of a tag the remote lacks: %d %v", r.status, r.header)
	}
	if r := dockerGet(u, "v2/demo/app/manifests/1.0"); r.status != 200 || string(r.body) != string(manifest) {
		t.Fatalf("GET after HEAD: %d %q", r.status, r.body)
	}
}

// With both ages 0 a manifest by tag is asked on every request, a manifest
// by digest and a blob never again.
func TestProxyDockerDigestsAreNeverCheckedAgain(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: dockerProxy, format: "docker",
		attributes: map[string]any{"proxy": map[string]any{"contentMaxAge": 0, "metadataMaxAge": 0}}}}})
	files := dockerImage("demo/app", "1.0", []byte("a layer"))
	remoteImage(u, dockerProxy, "demo/app", files, nil)
	_, digest, blobs := imageParts(files, "demo/app", "1.0")
	base := remoteBase(dockerProxy)
	for i := 0; i < 3; i++ {
		for _, path := range append([]string{"v2/demo/app/manifests/1.0", "v2/demo/app/manifests/" + digest}, "v2/demo/app/blobs/"+blobs[0]) {
			if r := dockerGet(u, path); r.status != 200 {
				t.Fatalf("pass %d, %s: %d %q", i+1, path, r.status, r.body)
			}
		}
	}
	if n := u.remote.Count(base + "v2/demo/app/manifests/1.0"); n != 3 {
		t.Errorf("%d requests of the tag with metadata age 0, want 3", n)
	}
	if n := u.remote.Count(base + "v2/demo/app/manifests/" + digest); n != 0 {
		t.Errorf("%d requests of the digest, which the tag stored, want 0", n)
	}
	if n := u.remote.Count(base + "v2/demo/app/blobs/" + blobs[0]); n != 1 {
		t.Errorf("%d requests of a blob with content age 0, want 1", n)
	}
}

// The tag list comes from the remote within the metadata age and joins the
// tags the cache holds; with the remote down the last list stays; a name the
// remote and the cache lack is unknown.
func TestProxyDockerTagList(t *testing.T) {
	cached := blobsOf(dockerProxy, dockerImage("demo/app", "0.9", []byte("an old layer")))
	u := startUpstream(t, upstreamSetup{upstream: true, blobs: cached, proxies: []proxyDef{{name: dockerProxy, format: "docker",
		attributes: map[string]any{"proxy": map[string]any{"metadataMaxAge": 0}, "httpclient": map[string]any{"autoBlock": false}}}}})
	base := remoteBase(dockerProxy)
	u.remote.Script(base+"v2/demo/app/tags/list", testkit.OK(`{"name":"demo/app","tags":["1.0","2.0"]}`).With("Content-Type", "application/json"),
		testkit.ServerError())
	tags := func() []string {
		r := dockerGet(u, "v2/demo/app/tags/list")
		var v struct{ Tags []string }
		if r.status != 200 || json.Unmarshal(r.body, &v) != nil {
			t.Fatalf("tag list: %d %q", r.status, r.body)
		}
		return v.Tags
	}
	want := []string{"0.9", "1.0", "2.0"}
	if got := tags(); !slices.Equal(got, want) {
		t.Fatalf("tags %v, want %v", got, want)
	}
	// metadata age 0: the next request asks again, the remote fails, the last list stays
	if got := tags(); !slices.Equal(got, want) {
		t.Fatalf("tags with the remote down %v, want %v", got, want)
	}
	if n := u.remote.Count(base + "v2/demo/app/tags/list"); n != 2 {
		t.Fatalf("%d requests of the tag list, want 2", n)
	}
	if r := dockerGet(u, "v2/demo/other/tags/list"); r.status != 404 || !strings.Contains(string(r.body), "NAME_UNKNOWN") {
		t.Fatalf("a name nobody knows: %d %q", r.status, r.body)
	}
}

// An n of a tag list that does not read answers 400 before the remote is
// asked, as the read path of a hosted repository checks n first; a valid n
// asks.
func TestProxyDockerTagListChecksNFirst(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: dockerProxy, format: "docker",
		attributes: map[string]any{"httpclient": map[string]any{"autoBlock": false}}}}})
	base := remoteBase(dockerProxy)
	u.remote.Script(base+"v2/demo/app/tags/list", testkit.OK(`{"name":"demo/app","tags":["1.0"]}`).With("Content-Type", "application/json"))
	for _, name := range []string{"demo/app", "demo/other"} {
		r := dockerGet(u, "v2/"+name+"/tags/list?n=x")
		if n := u.remote.Count(base + "v2/" + name + "/tags/list"); n != 0 {
			t.Fatalf("%s with n=x: %d requests at the remote, want 0", name, n)
		}
		if r.status != 400 || !strings.Contains(string(r.body), "PAGINATION_NUMBER_INVALID") {
			t.Fatalf("%s with n=x: %d %q", name, r.status, r.body)
		}
	}
	r := dockerGet(u, "v2/demo/app/tags/list?n=1")
	if r.status != 200 || !strings.Contains(string(r.body), `"1.0"`) {
		t.Fatalf("n=1: %d %q", r.status, r.body)
	}
	if n := u.remote.Count(base + "v2/demo/app/tags/list"); n != 1 {
		t.Fatalf("n=1: %d requests at the remote, want 1", n)
	}
}

// The token flow through the server: the remote asks for a bearer token
// from a realm on another listener, without credentials of the repository.
func TestProxyDockerTokenFlow(t *testing.T) {
	realm := testkit.NewRemote(t)
	realm.Script("/token", testkit.OK(`{"token":"demo-token-1","expires_in":300}`).With("Content-Type", "application/json"))
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: dockerProxy, format: "docker"}}})
	files := dockerImage("demo/app", "1.0", []byte("a layer behind a token"))
	guard := func(a testkit.Answer) testkit.Answer {
		a.Authorization = "Bearer demo-token-1"
		a.Challenge = `Bearer realm="` + realm.URL() + `/token",service="demo-registry",scope="repository:demo/app:pull"`
		return a
	}
	remoteImage(u, dockerProxy, "demo/app", files, guard)
	base := remoteBase(dockerProxy)
	u.remote.Script(base+"v2/demo/app/tags/list", guard(testkit.OK(`{"name":"demo/app","tags":["1.0","1.1"]}`)))
	u.remote.Script(base+"v2/demo/app/manifests/1.1", guard(testkit.OK(`{"schemaVersion": 2}`).With("Content-Type", dockerManifestType)))
	manifest, _, blobs := imageParts(files, "demo/app", "1.0")
	if r := dockerGet(u, "v2/demo/app/manifests/1.0"); r.status != 200 || string(r.body) != string(manifest) {
		t.Fatalf("manifest: %d %q", r.status, r.body)
	}
	for _, blob := range blobs {
		if r := dockerGet(u, "v2/demo/app/blobs/"+blob); r.status != 200 {
			t.Fatalf("blob %s: %d %q", blob, r.status, r.body)
		}
	}
	// the tag list and HEAD of an uncached manifest use the same client of the proxy, which keeps the token
	if r := dockerGet(u, "v2/demo/app/tags/list"); r.status != 200 || !strings.Contains(string(r.body), `"1.1"`) {
		t.Fatalf("tag list: %d %q", r.status, r.body)
	}
	if r := do(t, u.addr(), "HEAD", "/repository/"+dockerProxy+"/v2/demo/app/manifests/1.1", nil, "basic", asAdmin); r.status != 200 {
		t.Fatalf("HEAD: %d %v", r.status, r.header)
	}
	if n := len(realm.Requests()); n != 1 {
		t.Fatalf("%d token requests, want 1", n)
	}
	// the manifest took the challenge and its answer; each blob, the tag list and HEAD went at once with the token
	if n := len(u.remote.Requests()); n != 4+len(blobs) {
		t.Fatalf("%d requests at the remote: %+v", n, u.remote.Requests())
	}
}

// A blocked path of a Docker proxy answers 403 with the registry error and
// causes no request, with upstream on and off.
func TestProxyDockerRoutingRule(t *testing.T) {
	for _, upstream := range []bool{false, true} {
		u := startUpstream(t, upstreamSetup{upstream: upstream, proxies: []proxyDef{{name: dockerProxy, format: "docker"}},
			tune: func(o *appOptions) {
				o.routing = routingOf(dockerProxy, "^/v2/demo/blocked/.*")
			}})
		r := dockerGet(u, "v2/demo/blocked/manifests/1.0")
		if r.status != http.StatusForbidden || !strings.Contains(string(r.body), "DENIED") {
			t.Errorf("upstream %v: %d %q", upstream, r.status, r.body)
		}
		if n := len(u.remote.Requests()); n != 0 {
			t.Errorf("upstream %v: %d requests at the remote", upstream, n)
		}
	}
}

// routingOf is one routing rule that blocks the matchers given, named by
// the repository given (decision 5.15).
func routingOf(repo string, matchers ...string) *model.Routing {
	return &model.Routing{Rules: []model.RoutingRule{{Name: "demo-block-" + repo, Mode: "BLOCK", Matchers: matchers}},
		Links: map[string]string{repo: "demo-block-" + repo}}
}

// A Docker proxy with credentials: a name the account of the remote may not
// pull answers 401 to the token as well (insufficient_scope). That is a
// refusal of the name, so the proxy stays ready and serves the next name;
// only a realm that refuses the credentials puts it into needs secret.
func TestProxyDockerDeniedNameKeepsTheProxy(t *testing.T) {
	realm := testkit.NewRemote(t)
	realm.Script("/token", testkit.OK(`{"token":"demo-token-1","expires_in":300}`).With("Content-Type", "application/json"))
	realm.Script("/refused", testkit.Answer{Status: http.StatusUnauthorized})
	const refusing = "demo-docker-refusing"
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{
		{name: dockerProxy, format: "docker", attributes: authentication("username", false)},
		{name: refusing, format: "docker", attributes: authentication("username", false)}}})
	for _, name := range []string{dockerProxy, refusing} {
		if _, err := proxy.WriteSecret(u.cfg.DataDir, name, canaryPassword); err != nil {
			t.Fatal(err)
		}
	}
	challenge := func(path, scope string) string {
		return `Bearer realm="` + realm.URL() + path + `",service="demo-registry",scope="repository:` + scope + `:pull"`
	}
	guarded := func(a testkit.Answer, token, scope string) testkit.Answer {
		a.Authorization, a.Challenge = "Bearer "+token, challenge("/token", scope)
		return a
	}
	files := dockerImage("demo/app", "1.0", []byte("a layer the account may pull"))
	remoteImage(u, dockerProxy, "demo/app", files, func(a testkit.Answer) testkit.Answer { return guarded(a, "demo-token-1", "demo/app") })
	base := remoteBase(dockerProxy)
	// the token of the realm opens no path of demo/denied
	denied := guarded(testkit.OK(`{"schemaVersion": 2}`).With("Content-Type", dockerManifestType), "a-token-the-realm-never-gives", "demo/denied")
	for _, path := range []string{"v2/demo/denied/manifests/1.0", "v2/demo/denied/tags/list"} {
		u.remote.Script(base+path, denied)
	}
	if r := dockerGet(u, "v2/demo/denied/manifests/1.0"); r.status != http.StatusNotFound {
		t.Fatalf("manifest of the denied name: %d %q", r.status, r.body)
	}
	if r := do(t, u.addr(), "HEAD", "/repository/"+dockerProxy+"/v2/demo/denied/manifests/1.0", nil, "basic", asAdmin); r.status != http.StatusNotFound {
		t.Fatalf("HEAD of the denied name: %d %v", r.status, r.header)
	}
	if r := dockerGet(u, "v2/demo/denied/tags/list"); r.status != http.StatusNotFound {
		t.Fatalf("tag list of the denied name: %d %q", r.status, r.body)
	}
	if s := u.state(dockerProxy); s.Status != proxy.Ready {
		t.Fatalf("after a denied name: %+v", s)
	}
	manifest, _, _ := imageParts(files, "demo/app", "1.0")
	if r := dockerGet(u, "v2/demo/app/manifests/1.0"); r.status != 200 || string(r.body) != string(manifest) {
		t.Fatalf("the next name: %d %q", r.status, r.body)
	}
	// a realm that forbids one scope (403), and one on another origin over
	// http that answers 401 to a request without the credentials, refuse
	// that name only (finding 5-4 of review 5)
	realm.Script("/forbidden", testkit.Answer{Status: http.StatusForbidden})
	for _, c := range []struct{ name, realmPath string }{{"demo/forbidden", "/forbidden"}, {"demo/uncarried", "/refused"}} {
		u.remote.Script(base+"v2/"+c.name+"/manifests/1.0", testkit.Answer{Status: http.StatusOK, Authorization: "Bearer none",
			Challenge: challenge(c.realmPath, c.name)})
		if r := dockerGet(u, "v2/"+c.name+"/manifests/1.0"); r.status != http.StatusNotFound {
			t.Fatalf("%s: %d %q", c.name, r.status, r.body)
		}
		if s := u.state(dockerProxy); s.Status != proxy.Ready {
			t.Fatalf("after %s: %+v", c.name, s)
		}
	}
	if len(realm.Requests()) == 0 || realm.Requests()[len(realm.Requests())-1].Header.Get("Authorization") != "" {
		t.Fatal("the realm on another origin over http got the credentials")
	}
	// a realm of the origin of the repository that answers 401 to the credentials: needs secret
	u.remote.Script(remoteBase(refusing)+"token", testkit.Answer{Status: http.StatusUnauthorized})
	u.remote.Script(remoteBase(refusing)+"v2/demo/app/manifests/1.0", testkit.Answer{Status: http.StatusOK, Authorization: "Bearer none",
		Challenge: `Bearer realm="` + u.remote.URL() + remoteBase(refusing) + `token",service="demo-registry",scope="repository:demo/app:pull"`})
	if r := u.get(refusing, "v2/demo/app/manifests/1.0"); r.status != http.StatusNotFound {
		t.Fatalf("a refused realm: %d %q", r.status, r.body)
	}
	if s := u.state(refusing); s.Status != proxy.NeedsSecret {
		t.Fatalf("after a refused realm: %+v", s)
	}
}

// A manifest of the remote is checked as a pushed one is: a body that is no
// manifest of the four types answers 502 and is not stored, and the type
// stored and served is the one of the manifest, never another Content-Type
// of the remote.
func TestProxyDockerChecksTheManifestOfTheRemote(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: dockerProxy, format: "docker"}}})
	base := remoteBase(dockerProxy)
	u.remote.Script(base+"v2/demo/page/manifests/1.0", testkit.OK(`<html><script>alert(1)</script></html>`).With("Content-Type", "text/html"))
	u.remote.Script(base+"v2/demo/one/manifests/1.0", testkit.OK(`{"schemaVersion": 1, "name": "demo/one"}`).With("Content-Type", "application/json"))
	for _, path := range []string{"v2/demo/page/manifests/1.0", "v2/demo/one/manifests/1.0"} {
		if r := dockerGet(u, path); r.status != http.StatusBadGateway {
			t.Errorf("%s: %d %v %q", path, r.status, r.header, r.body)
		}
	}
	if blobs, work := u.blobFiles(); len(blobs) != 0 || len(work) != 0 {
		t.Fatalf("blobs %v, work files %v", blobs, work)
	}
	// a manifest the remote sends as application/octet-stream is stored and served with the type of its document
	files := dockerImage("demo/app", "1.0", []byte("a layer"))
	manifest, _, _ := imageParts(files, "demo/app", "1.0")
	u.remote.Script(base+"v2/demo/app/manifests/1.0", testkit.OK(string(manifest)))
	r := dockerGet(u, "v2/demo/app/manifests/1.0")
	if r.status != 200 || string(r.body) != string(manifest) || r.header.Get("Content-Type") != dockerManifestType {
		t.Fatalf("a manifest as octet-stream: %d %v %q", r.status, r.header, r.body)
	}
}

// A blob is fetched under the image name of the request but kept under the
// shared blob path. A 404 of the remote for one name is no answer for
// another name: it leaves no not found mark on the shared path, and the
// next name fetches the blob.
func TestProxyDockerBlobMissingUnderOneName(t *testing.T) {
	u, _ := notFoundApp(t, []proxyDef{{name: dockerProxy, format: "docker", attributes: notFoundCache(true, 1440)}})
	layer := []byte("a layer of demo/app only")
	u.remote.Script(remoteBase(dockerProxy)+"v2/demo/app/blobs/"+digestOf(layer), testkit.OK(string(layer)))
	if r := do(t, u.addr(), "HEAD", "/repository/"+dockerProxy+"/v2/demo/other/blobs/"+digestOf(layer), nil, "basic", asAdmin); r.status != http.StatusNotFound {
		t.Fatalf("HEAD under another name: %d %v", r.status, r.header)
	}
	if r := dockerGet(u, "v2/demo/other/blobs/"+digestOf(layer)); r.status != http.StatusNotFound {
		t.Fatalf("GET under another name: %d %q", r.status, r.body)
	}
	if r := dockerGet(u, "v2/demo/app/blobs/"+digestOf(layer)); r.status != 200 || string(r.body) != string(layer) {
		t.Fatalf("GET under its name after a 404 under another: %d %q", r.status, r.body)
	}
}

// A Docker proxy with a static bearer secret sent at once: a 401 to it
// refuses the secret, as for the other formats (finding 5-5 of review 5).
func TestProxyDockerWrongStaticBearer(t *testing.T) {
	const bearer = "demo-docker-bearer"
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{
		{name: bearer, format: "docker", attributes: authentication("bearerToken", true)}}})
	if _, err := proxy.WriteSecret(u.cfg.DataDir, bearer, canaryWrong); err != nil {
		t.Fatal(err)
	}
	u.remote.Script(remoteBase(bearer)+"v2/demo/app/manifests/1.0", testkit.Answer{Status: http.StatusUnauthorized})
	if r := u.get(bearer, "v2/demo/app/manifests/1.0"); r.status != http.StatusNotFound {
		t.Fatalf("a wrong bearer secret: %d %q", r.status, r.body)
	}
	if s := u.state(bearer); s.Status != proxy.NeedsSecret {
		t.Fatalf("after a wrong bearer secret: %+v", s)
	}
}
