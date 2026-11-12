package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

const v2 = "/repository/demo-docker/v2"

func errorCode(t *testing.T, r *response) string {
	t.Helper()
	if r.header.Get("Content-Type") != "application/json" || r.header.Get("Docker-Distribution-Api-Version") != "registry/2.0" ||
		r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("error headers %v", r.header)
	}
	var doc struct {
		Errors []struct{ Code, Message string } `json:"errors"`
	}
	if err := json.Unmarshal(r.body, &doc); err != nil || len(doc.Errors) != 1 || doc.Errors[0].Message == "" {
		t.Fatalf("error body %q: %v", r.body, err)
	}
	return doc.Errors[0].Code
}

func dockerApp(t *testing.T, files map[string]file, extra ...testkit.BlobSpec) *app {
	t.Helper()
	base := t.TempDir()
	blobs := append(blobsOf("demo-docker", files), extra...)
	return newApp(t, []config.StoreConfig{blobStore(t, base, "s1", blobs...)},
		appOptions{defs: []*pyjson.Object{def("demo-docker", "docker", "hosted")}})
}

func TestDockerBaseAndManifests(t *testing.T) {
	files := dockerImage("demo/app", "1.0", []byte("layer one"), []byte("layer two"))
	manifest := files["v2/demo/app/manifests/1.0"].data
	digest := digestOf(manifest)
	other := []byte(`{"schemaVersion": 2, "layers": [], "note": "bytes that hash to another digest"}`)
	wrong := "sha256:" + strings.Repeat("0", 63) + "1"
	files["v2/demo/app/manifests/"+wrong] = file{other, dockerManifestType}
	a := dockerApp(t, files)
	for _, url := range []string{v2 + "/", v2} {
		r := a.get(url)
		expect(t, r, 200)
		if string(r.body) != "{}" || r.header.Get("Docker-Distribution-Api-Version") != "registry/2.0" ||
			r.header.Get("Content-Type") != "application/json" || r.header.Get("Content-Length") != "2" {
			t.Fatalf("%s: %s", url, r)
		}
	}
	get := a.get(v2+"/demo/app/manifests/1.0", "Accept", "text/html")
	expect(t, get, 200)
	if string(get.body) != string(manifest) || get.header.Get("Content-Type") != dockerManifestType ||
		get.header.Get("Docker-Content-Digest") != digest || get.header.Get("ETag") != `"`+digest+`"` ||
		get.header.Get("Content-Length") != fmt.Sprint(len(manifest)) {
		t.Fatalf("%s", get)
	}
	head := a.head(v2 + "/demo/app/manifests/1.0")
	if head.status != 200 || len(head.body) != 0 || head.header.Get("Docker-Content-Digest") != digest {
		t.Fatalf("%s", head)
	}
	byDigest := a.get(v2 + "/demo/app/manifests/" + digest)
	if byDigest.status != 200 || byDigest.header.Get("Docker-Content-Digest") != digest {
		t.Fatalf("%s", byDigest)
	}
	for _, method := range []string{"GET", "HEAD"} {
		bad := raw(t, a.addr(), method, v2+"/demo/app/manifests/"+wrong)
		expect(t, bad, 404)
		if bad.header.Get("Docker-Content-Digest") != "" || strings.Contains(string(bad.body), "another digest") {
			t.Fatalf("%s", bad)
		}
	}
	if code := errorCode(t, a.get(v2+"/demo/app/manifests/"+wrong)); code != "MANIFEST_UNKNOWN" {
		t.Fatal(code)
	}
	notModified := a.get(v2+"/demo/app/manifests/1.0", "If-None-Match", `"`+digest+`"`)
	if notModified.status != 304 || notModified.header.Get("Docker-Content-Digest") != digest {
		t.Fatalf("%s", notModified)
	}
}

func TestDockerSchema1ByDigest(t *testing.T) {
	body := []byte(`{"schemaVersion": 1, "name": "demo/app", "tag": "0.9", "fsLayers": [], "signatures": [{"protected": "x"}]}`)
	digest := "sha256:" + strings.Repeat("ab", 32)
	files := map[string]file{
		"v2/demo/app/manifests/" + digest: {body, schema1Type},
		"v2/demo/app/manifests/0.9":       {body, schema1Type},
	}
	a := dockerApp(t, files)
	for _, method := range []string{"GET", "HEAD"} {
		r := raw(t, a.addr(), method, v2+"/demo/app/manifests/"+digest)
		expect(t, r, 200)
		if r.header.Get("Docker-Content-Digest") != digest || r.header.Get("Content-Type") != schema1Type {
			t.Fatalf("%s", r)
		}
	}
	if string(a.get(v2+"/demo/app/manifests/0.9").body) != string(body) {
		t.Fatal("schema 1 by tag")
	}
}

func TestDockerMediaTypeFromBytes(t *testing.T) {
	index := []byte(`{"schemaVersion": 2, "manifests": []}`)
	a := dockerApp(t, nil, spec("demo-docker", "v2/demo/tools/cli/manifests/2.0", index, ""))
	r := a.get(v2 + "/demo/tools/cli/manifests/2.0")
	expect(t, r, 200)
	if r.header.Get("Content-Type") != ociIndexType || r.header.Get("Docker-Content-Digest") != digestOf(index) {
		t.Fatalf("%s", r)
	}
}

func TestDockerBlobs(t *testing.T) {
	files := dockerImage("demo/app", "1.0", []byte("layer one"))
	stored := []byte("bytes that do not hash to the name of their path")
	name := "sha256:" + strings.Repeat("c", 64)
	files["v2/-/blobs/"+name] = file{stored, ""}
	a := dockerApp(t, files)
	layer := []byte("layer one")
	url := v2 + "/demo/app/blobs/" + digestOf(layer)
	get := a.get(url)
	expect(t, get, 200)
	if string(get.body) != string(layer) || get.header.Get("Docker-Content-Digest") != digestOf(layer) ||
		get.header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("%s", get)
	}
	ranged := a.get(url, "Range", "bytes=0-3")
	if ranged.status != 206 || string(ranged.body) != "laye" || ranged.header.Get("Content-Range") != fmt.Sprintf("bytes 0-3/%d", len(layer)) {
		t.Fatalf("%s", ranged)
	}
	if other := a.get(v2 + "/demo/tools/cli/blobs/" + digestOf(layer)); other.status != 200 || string(other.body) != string(layer) {
		t.Fatalf("%s", other)
	}
	for _, method := range []string{"GET", "HEAD"} {
		r := raw(t, a.addr(), method, v2+"/demo/app/blobs/"+name)
		if r.status != 200 || r.header.Get("Docker-Content-Digest") != name || r.header.Get("Content-Length") != fmt.Sprint(len(stored)) {
			t.Fatalf("%s", r)
		}
	}
	unsatisfiable := a.get(url, "Range", "bytes=100-")
	if unsatisfiable.status != 416 || unsatisfiable.header.Get("Docker-Content-Digest") != digestOf(layer) {
		t.Fatalf("%s", unsatisfiable)
	}
}

func TestDockerTags(t *testing.T) {
	files := map[string]file{}
	for _, tag := range []string{"2.0", "1.0", "10.0"} {
		for path, f := range dockerImage("demo/app", tag, []byte("layer "+tag)) {
			files[path] = f
		}
	}
	a := dockerApp(t, files)
	url := v2 + "/demo/app/tags/list"
	tags := func(r *response) string {
		t.Helper()
		var doc struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(r.body, &doc); err != nil || doc.Name != "demo/app" {
			t.Fatalf("%s %v", r, err)
		}
		return strings.Join(doc.Tags, ",")
	}
	full := a.get(url)
	if full.status != 200 || string(full.body) != `{"name": "demo/app", "tags": ["1.0", "10.0", "2.0"]}` || full.header.Get("Link") != "" {
		t.Fatalf("%s", full)
	}
	first := a.get(url + "?n=2")
	if tags(first) != "1.0,10.0" || first.header.Get("Link") != `</repository/demo-docker/v2/demo/app/tags/list?n=2&last=10.0>; rel="next"` {
		t.Fatalf("%s", first)
	}
	if r := a.get(url + "?last=10.0"); tags(r) != "2.0" || r.header.Get("Link") != "" {
		t.Fatalf("%s", r)
	}
	if r := a.get(url + "?n=1&last=1.0"); tags(r) != "10.0" || !strings.Contains(r.header.Get("Link"), `rel="next"`) {
		t.Fatalf("%s", r)
	}
	if r := a.get(url + "?n=3"); tags(r) != "1.0,10.0,2.0" || r.header.Get("Link") != "" {
		t.Fatalf("%s", r)
	}
	if r := a.get(url + "?n=0"); r.status != 200 || tags(r) != "" || r.header.Get("Link") != "" {
		t.Fatalf("%s", r)
	}
	if r := a.get(url + "?n=999999999999999999999999"); tags(r) != "1.0,10.0,2.0" {
		t.Fatalf("%s", r)
	}
	bad := a.get(url + "?n=x")
	expect(t, bad, 400)
	if errorCode(t, bad) != "PAGINATION_NUMBER_INVALID" {
		t.Fatalf("%s", bad)
	}
	if head := a.head(url); head.status != 200 || len(head.body) != 0 || head.header.Get("Content-Length") != fmt.Sprint(len(full.body)) {
		t.Fatalf("%s", head)
	}
}

func TestDockerErrors(t *testing.T) {
	a := dockerApp(t, dockerImage("demo/app", "1.0", []byte("layer one")))
	zero := "sha256:" + strings.Repeat("0", 64)
	for _, c := range []struct{ url, code string }{
		{v2 + "/demo/other/manifests/1.0", "NAME_UNKNOWN"},
		{v2 + "/demo/other/tags/list", "NAME_UNKNOWN"},
		{v2 + "/demo/app/manifests/9.9", "MANIFEST_UNKNOWN"},
		{v2 + "/demo/app/manifests/" + zero, "MANIFEST_UNKNOWN"},
		{v2 + "/demo/app/blobs/" + zero, "BLOB_UNKNOWN"},
		{v2 + "/_catalog", "UNSUPPORTED"},
	} {
		r := a.get(c.url)
		expect(t, r, 404)
		if code := errorCode(t, r); code != c.code {
			t.Errorf("%s: %s, want %s", c.url, code, c.code)
		}
	}
	for _, c := range []struct{ path, code string }{
		{"Demo/app/manifests/1.0", "NAME_INVALID"},
		{"demo/app/manifests/-bad", "MANIFEST_INVALID"},
		{"demo/app/blobs/not-a-digest", "DIGEST_INVALID"},
		{"demo/app/manifests/" + strings.Repeat("t", 129), "MANIFEST_INVALID"},
	} {
		r := a.get(v2 + "/" + c.path)
		expect(t, r, 400)
		if code := errorCode(t, r); code != c.code {
			t.Errorf("%s: %s, want %s", c.path, code, c.code)
		}
	}
	for _, path := range []string{"demo/../app/manifests/1.0", "demo/%2e%2e/app/manifests/1.0", "demo//app/manifests/1.0",
		"demo/app/manifests/%2e%2e", "demo/app/tags/list/../../../secret/tags/list"} {
		r := a.get(v2 + "/" + path)
		expect(t, r, 400)
		if !strings.HasPrefix(string(r.body), "Bad request path:") {
			t.Fatalf("%s: %s", path, r)
		}
	}
	expect(t, a.get("/repository/demo-docker"), 404)
	expect(t, a.get("/repository/demo-docker/a.txt"), 404)
	for _, c := range []struct{ method, url string }{
		{"POST", v2 + "/demo/app/blobs/uploads/"}, {"PATCH", v2 + "/demo/app/blobs/uploads/abc"},
		{"PUT", v2 + "/demo/app/manifests/2.0"}, {"DELETE", v2 + "/demo/app/manifests/1.0"},
	} {
		r := raw(t, a.addr(), c.method, c.url, "Content-Length", "0")
		expect(t, r, 405)
		if errorCode(t, r) != "UNSUPPORTED" || r.header.Get("Allow") != "GET, HEAD" {
			t.Fatalf("%s", r)
		}
	}
}

func TestDockerGroupTriesEveryMember(t *testing.T) {
	files := dockerImage("demo/app", "1.0", []byte("layer one"))
	manifest := files["v2/demo/app/manifests/1.0"].data
	digest := digestOf(manifest)
	wrong := map[string]file{}
	for path, f := range files {
		wrong[path] = f
	}
	wrong["v2/demo/app/manifests/"+digest] = file{append(append([]byte{}, manifest...), ' '), dockerManifestType}
	base := t.TempDir()
	blobs := append(blobsOf("demo-docker", wrong), blobsOf("demo-docker-old", files)...)
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", blobs...)}, appOptions{defs: []*pyjson.Object{
		def("demo-docker", "docker", "hosted"), def("demo-docker-old", "docker", "hosted"),
		def("demo-docker-group", "docker", "group", "demo-docker", "demo-docker-old")}})
	r := a.get("/repository/demo-docker-group/v2/demo/app/manifests/" + digest)
	if r.status != 200 || string(r.body) != string(manifest) || r.header.Get("Docker-Content-Digest") != digest {
		t.Fatalf("%s", r)
	}
	expect(t, a.get("/repository/demo-docker/v2/demo/app/manifests/"+digest), 404)
	if string(a.get("/repository/demo-docker-group/v2/demo/app/manifests/1.0").body) != string(manifest) {
		t.Fatal("by tag")
	}
}

func TestDockerPorts(t *testing.T) {
	base := t.TempDir()
	app1 := dockerImage("demo/app", "1.0", []byte("layer one"))
	old := dockerImage("demo/old", "1.0", []byte("old layer"))
	blobs := append(blobsOf("demo-docker", app1), blobsOf("demo-docker-old", old)...)
	cfg := prepare(t, base, []config.StoreConfig{blobStore(t, base, "s1", blobs...)}, appOptions{defs: []*pyjson.Object{
		def("demo-docker", "docker", "hosted"), def("demo-docker-old", "docker", "hosted"), def("demo-npm", "npm", "hosted")}})
	a := startApp(t, base, cfg, appOptions{dockerRepos: []string{"demo-docker", "demo-docker-old", "demo-npm"}})
	manifest := app1["v2/demo/app/manifests/1.0"].data
	r := a.on("demo-docker", "GET", "/v2/")
	if r.status != 200 || string(r.body) != "{}" {
		t.Fatalf("%s", r)
	}
	r = a.on("demo-docker", "GET", "/v2/demo/app/manifests/1.0")
	if r.status != 200 || string(r.body) != string(manifest) {
		t.Fatalf("%s", r)
	}
	expect(t, a.on("demo-docker", "GET", "/v2/demo/old/manifests/1.0"), 404)
	expect(t, a.on("demo-docker-old", "GET", "/v2/demo/app/tags/list"), 404)
	for _, path := range []string{"/repository/demo-docker/v2/", "/api/health", "/", "/service/rest/v1/status", "/V2/", "/v2x/"} {
		r := a.on("demo-docker", "GET", path)
		expect(t, r, 404)
		if errorCode(t, r) != "UNSUPPORTED" || !strings.Contains(string(r.body), "/v2/") {
			t.Fatalf("%s: %s", path, r)
		}
	}
	for _, path := range []string{"/v2/../api/health", "/v2/%2e%2e/repository/demo-docker/v2/demo/app/manifests/1.0"} {
		r := a.on("demo-docker", "GET", path)
		if (r.status != 400 && r.status != 404) || strings.Contains(string(r.body), "layer") {
			t.Fatalf("%s: %s", path, r)
		}
	}
	tags := a.on("demo-docker", "GET", "/v2/demo/app/tags/list?n=0")
	expect(t, tags, 200)
	write := a.on("demo-docker", "PUT", "/v2/demo/app/manifests/2.0", "Content-Length", "0")
	if write.status != 405 || errorCode(t, write) != "UNSUPPORTED" {
		t.Fatalf("%s", write)
	}
	for _, path := range []string{"/v2/", "/v2/demo-lib", "/v2/demo/app/manifests/1.0"} {
		r := a.on("demo-npm", "GET", path)
		expect(t, r, 404)
		if errorCode(t, r) != "NAME_UNKNOWN" {
			t.Fatalf("%s: %s", path, r)
		}
	}
	// the definition changes while the server runs: the port keeps its repository name
	if err := repos.WriteDefinitions(cfg.RepositoriesPath(), []*pyjson.Object{def("demo-docker", "docker", "hosted")}); err != nil {
		t.Fatal(err)
	}
	// demo-docker-old keeps its hint from the index; demo-npm is gone
	expect(t, a.on("demo-docker-old", "GET", "/v2/"), 200)
	if code := errorCode(t, a.on("demo-npm", "GET", "/v2/")); code != "NAME_UNKNOWN" {
		t.Fatal(code)
	}
	expect(t, a.get("/repository/demo-docker/v2/demo/app/manifests/1.0"), 200)
	expect(t, a.on("demo-docker", "GET", "/v2"), 200)
	expect(t, a.on("demo-docker", "GET", "/api/health", "Host", "evil.example"), 403)
	line := a.access.String()
	if !strings.Contains(line, `"GET /v2/demo/app/manifests/1.0 HTTP/1.1" 200 OK`) {
		t.Fatalf("access log of the Docker port: %s", line)
	}
}

func TestDockerTagsLinkOnTheDockerPort(t *testing.T) {
	base := t.TempDir()
	files := map[string]file{}
	for _, tag := range []string{"1.0", "2.0"} {
		for path, f := range dockerImage("demo/app", tag, []byte("layer "+tag)) {
			files[path] = f
		}
	}
	cfg := prepare(t, base, []config.StoreConfig{blobStore(t, base, "s1", blobsOf("demo-docker", files)...)},
		appOptions{defs: []*pyjson.Object{def("demo-docker", "docker", "hosted")}})
	a := startApp(t, base, cfg, appOptions{dockerRepos: []string{"demo-docker"}})
	r := a.on("demo-docker", "GET", "/v2/demo/app/tags/list?n=1")
	if r.header.Get("Link") != `</v2/demo/app/tags/list?n=1&last=1.0>; rel="next"` {
		t.Fatalf("%s", r)
	}
}
