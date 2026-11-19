package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The Docker writes of leg 4c (tasks 13 and 14 of the phase 4 plan) through
// the real handlers, with the fake security of secure_test.go. The black
// box proofs are tests/write/test_docker.py.

const (
	asPusher = "demo-pusher:pusher-pw" // add, read and browse of demo-docker only
	asAdder  = "demo-adder:adder-pw"   // add and read of every repository, no edit, no delete
)

var sessionLocation = regexp.MustCompile(`^(/repository/demo-docker)?/v2/app/blobs/uploads/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// startOCI is a server with writes on over store-a of the old product, with
// the hosted repositories demo-docker and demo-docker-2 on Docker ports of
// their own, demo-npm, and demo-raw, whose stored attributes are given.
func startOCI(t *testing.T, attributes map[string]map[string]any, blobs ...testkit.BlobSpec) *writing {
	t.Helper()
	sec := newFakeSecurity()
	sec.addBasic(asPusher, subject("demo-pusher", "", rest.ViewPermission("docker", "demo-docker", "add"),
		rest.ViewPermission("docker", "demo-docker", "read")))
	sec.addBasic(asAdder, subject("demo-adder", "", "nexus:repository-view:*:*:add,read,browse"))
	snap := *sec.snap
	snap.Repositories = nil
	var defs []*pyjson.Object
	for _, r := range [][2]string{{"demo-docker", "docker"}, {"demo-docker-2", "docker"}, {"demo-npm", "npm"}, {"demo-raw", "raw"}} {
		attrs := attributes[r[0]]
		if attrs == nil {
			attrs = storage("ALLOW")
		}
		snap.Repositories = append(snap.Repositories, model.Repository{Name: r[0], Format: r[1], Type: "hosted", Online: true, Attributes: attrs})
		defs = append(defs, def(r[0], r[1], "hosted"))
	}
	sec.snap = &snap
	base := t.TempDir()
	storeCfgs := []config.StoreConfig{blobStore(t, base, "store-a", blobs...)}
	w := &writing{catalog: &fakeCatalog{}, clock: &clock{now: time.Date(2026, 11, 18, 10, 15, 30, 123_000_000, time.UTC)},
		old: storeCfgs[0].Root}
	opts := appOptions{defs: defs, dockerRepos: []string{"demo-docker", "demo-docker-2"}, security: sec, writes: true,
		catalog: w.catalog, writeOptions: blobwrite.Options{Now: w.clock.Now}}
	w.app = startApp(t, base, prepare(t, base, storeCfgs, opts), opts)
	return w
}

// servedByOCI reports a write case of write_test.go that a handler of leg
// 4c answers with writes on, so that it no longer answers the refusal of
// the port: every call below v2/ of a Docker repository and every npm path
// outside "-".
func servedByOCI(c writeCase) bool {
	return c.docker != "" || strings.HasPrefix(c.target, "/repository/demo-docker/v2") ||
		strings.HasPrefix(c.target, "/repository/demo-npm/") && !strings.HasPrefix(c.target, "/repository/demo-npm/-/")
}

// port is the address of the Docker port of a repository.
func (w *writing) port(repo string) string { return w.docker[repo].Listener.Addr().String() }

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// startUploadAt starts a session on the Docker port of demo-docker.
func (w *writing) startUploadAt(t *testing.T, who string) string {
	t.Helper()
	r := do(t, w.port("demo-docker"), "POST", "/v2/app/blobs/uploads/", nil, "basic", who)
	if r.status != http.StatusAccepted || !sessionLocation.MatchString(r.header.Get("Location")) {
		t.Fatalf("POST: %d %v %q", r.status, r.header, r.body)
	}
	return r.header.Get("Location")
}

// Risk 1 of report 4a: a write on a Docker port reaches the Docker handler,
// and the same call on the main port too, each with the Location of its port.
func TestDockerWriteReachesTheHandlerOnBothPorts(t *testing.T) {
	w := startOCI(t, nil)
	r := do(t, w.port("demo-docker"), "POST", "/v2/app/blobs/uploads/", nil, "basic", asAdmin)
	location := r.header.Get("Location")
	if r.status != http.StatusAccepted || !strings.HasPrefix(location, "/v2/app/blobs/uploads/") || !sessionLocation.MatchString(location) ||
		r.header.Get("Range") != "0-0" || r.header.Get("Docker-Upload-Uuid") != location[strings.LastIndex(location, "/")+1:] {
		t.Fatalf("the Docker port: %d %v %q", r.status, r.header, r.body)
	}
	main := do(t, w.addr(), "POST", "/repository/demo-docker/v2/app/blobs/uploads/", nil, "basic", asAdmin)
	if main.status != http.StatusAccepted || !strings.HasPrefix(main.header.Get("Location"), "/repository/demo-docker/v2/app/blobs/uploads/") {
		t.Fatalf("the main port: %d %v %q", main.status, main.header, main.body)
	}
	if n := w.srv.sessionCount(); n != 2 {
		t.Fatalf("%d sessions", n)
	}
}

// A session ends after an hour without a request (W3); at most
// maxUserSessions are open at once for one user, and maxUploadSessions in
// the process (assessment A4 of review 4).
func TestDockerSessionsAreBoundedAndEndWhenIdle(t *testing.T) {
	w := startOCI(t, nil)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	w.srv.oci.mu.Lock()
	w.srv.oci.now = func() time.Time { return now }
	w.srv.oci.mu.Unlock()
	advance := func(d time.Duration) {
		w.srv.oci.mu.Lock()
		now = now.Add(d)
		w.srv.oci.mu.Unlock()
	}
	first := w.startUploadAt(t, asAdmin)
	if r := do(t, w.port("demo-docker"), "PATCH", first, []byte("kept bytes"), "basic", asAdmin); r.status != http.StatusAccepted {
		t.Fatalf("PATCH: %d %q", r.status, r.body)
	}
	if len(w.workFiles()) != 1 {
		t.Fatalf("work files %v", w.workFiles())
	}
	for i := 1; i < maxUserSessions; i++ {
		w.startUploadAt(t, asAdmin)
	}
	mine := do(t, w.port("demo-docker"), "POST", "/v2/app/blobs/uploads/", nil, "basic", asAdmin)
	if mine.status != http.StatusTooManyRequests || !strings.Contains(string(mine.body), "TOOMANYREQUESTS") ||
		!strings.Contains(string(mine.body), "you hold "+strconv.Itoa(maxUserSessions)) {
		t.Fatalf("session %d of one user: %d %q", maxUserSessions+1, mine.status, mine.body)
	}
	// other users fill the process
	sec := w.srv.sec.(*fakeSecurity)
	pusher := func(n int) string {
		pair := "demo-push-" + strconv.Itoa(n) + ":push-pw"
		sec.addBasic(pair, subject("demo-push-"+strconv.Itoa(n), "", rest.ViewPermission("docker", "demo-docker", "add")))
		return pair
	}
	for n := 1; w.srv.sessionCount() < maxUploadSessions; n++ {
		who := pusher(n)
		for i := 0; i < maxUserSessions && w.srv.sessionCount() < maxUploadSessions; i++ {
			w.startUploadAt(t, who)
		}
	}
	full := do(t, w.port("demo-docker"), "POST", "/v2/app/blobs/uploads/", nil, "basic", pusher(100))
	if full.status != http.StatusTooManyRequests || !strings.Contains(string(full.body), strconv.Itoa(maxUploadSessions)+" upload sessions are open") {
		t.Fatalf("session %d: %d %q", maxUploadSessions+1, full.status, full.body)
	}
	advance(uploadIdle - time.Second)
	if r := do(t, w.port("demo-docker"), "GET", first, nil, "basic", asAdmin); r.status != http.StatusNoContent || r.header.Get("Range") != "0-9" {
		t.Fatalf("status before the hour: %d %v", r.status, r.header)
	}
	advance(uploadIdle - time.Second)
	// every session but the first rested for the hour; the start sweeps them
	w.startUploadAt(t, asAdmin)
	if n := w.srv.sessionCount(); n != 2 {
		t.Fatalf("%d sessions after the hour", n)
	}
	advance(time.Second)
	w.startUploadAt(t, asAdmin)
	if r := do(t, w.port("demo-docker"), "GET", first, nil, "basic", asAdmin); r.status != http.StatusNotFound {
		t.Fatalf("status after the hour: %d %q", r.status, r.body)
	}
	if len(w.workFiles()) != 0 {
		t.Fatalf("the work file of an ended session stays: %v", w.workFiles())
	}
}

// A stop of the server ends every upload session and removes its work file,
// so that content/tmp of the write store is empty once the writer is at
// rest (W2), as the layout checker of task 8 asks; the next start removed
// such files before, the stop left them.
func TestAStopEndsEveryUploadSession(t *testing.T) {
	w := startOCI(t, nil)
	location := w.startUploadAt(t, asAdmin)
	if r := do(t, w.port("demo-docker"), "PATCH", location, []byte("kept bytes"), "basic", asAdmin); r.status != http.StatusAccepted {
		t.Fatalf("PATCH: %d %q", r.status, r.body)
	}
	if len(w.workFiles()) != 1 {
		t.Fatalf("work files %v", w.workFiles())
	}
	w.ts.Close()
	for _, ts := range w.docker {
		ts.Close()
	}
	if err := w.srv.Close(); err != nil {
		t.Fatal(err)
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files after the stop: %v", files)
	}
}

// rawSend writes a request head and part of a body on a new connection and
// closes it.
func rawSend(t *testing.T, addr, head string, body []byte) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(append([]byte(head), body...)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
}

// eventually polls a condition for at most five seconds.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within 5 s", what)
}

// The digests of a body that names its blob, for a PUT of a test.
func digestQuery(data []byte) string { return "?digest=" + sha256Digest(data) }

// pushImage puts the config and the layer of a manifest into demo-docker
// and returns the manifest.
func (w *writing) pushImage(t *testing.T, note string) []byte {
	t.Helper()
	config := []byte(`{"architecture": "amd64", "os": "linux", "note": "` + note + `"}`)
	layer := []byte("the layer of " + note)
	for _, blob := range [][]byte{config, layer} {
		r := do(t, w.port("demo-docker"), "POST", "/v2/app/blobs/uploads/"+digestQuery(blob), blob, "basic", asAdmin)
		if r.status != http.StatusCreated {
			t.Fatalf("blob: %d %q", r.status, r.body)
		}
	}
	return []byte(`{"schemaVersion": 2, "mediaType": "` + dockerManifestType + `", "config": {"mediaType": "` + dockerConfigType +
		`", "size": ` + itoa(len(config)) + `, "digest": "` + sha256Digest(config) + `"}, "layers": [{"mediaType": "` + dockerLayerType +
		`", "size": ` + itoa(len(layer)) + `, "digest": "` + sha256Digest(layer) + `"}]}`)
}

func itoa(n int) string { return strconv.Itoa(n) }

// A manifest by tag is a blob of the digest and a hard link of it for the
// tag, each with its row and its catalog row; under ALLOW_ONCE a second
// push of the tag is refused before the digest blob of the new bytes is
// written.
func TestDockerManifestTagIsAHardLinkOfTheDigest(t *testing.T) {
	w := startOCI(t, map[string]map[string]any{"demo-docker": storage("ALLOW_ONCE")})
	manifest := w.pushImage(t, "one")
	digest := sha256Digest(manifest)
	r := do(t, w.port("demo-docker"), "PUT", "/v2/app/manifests/1.0", manifest, "basic", asAdmin, "Content-Type", dockerManifestType)
	if r.status != http.StatusCreated || r.header.Get("Docker-Content-Digest") != digest || r.header.Get("Location") != "/v2/app/manifests/"+digest {
		t.Fatalf("PUT: %d %v %q", r.status, r.header, r.body)
	}
	tag, _ := w.srv.rp.index.Lookup("demo-docker", "v2/app/manifests/1.0")
	byDigest, _ := w.srv.rp.index.Lookup("demo-docker", "v2/app/manifests/"+digest)
	if tag == nil || byDigest == nil || tag.Store != "local-write" || tag.Location == byDigest.Location {
		t.Fatalf("rows %+v %+v", tag, byDigest)
	}
	a, errA := os.Stat(filepath.Join(w.root(), tag.Location))
	b, errB := os.Stat(filepath.Join(w.root(), byDigest.Location))
	if errA != nil || errB != nil || !os.SameFile(a, b) {
		t.Fatalf("the tag blob is no hard link of the digest blob: %v %v", errA, errB)
	}
	if props := w.properties(tag.Location); props["@BlobStore.blob-name"] != "/v2/app/manifests/1.0" || props["@BlobStore.content-type"] != dockerManifestType {
		t.Fatalf("%q", props)
	}
	found := 0
	for _, p := range w.catalog.puts {
		if strings.HasPrefix(p, "demo-docker/v2/app/manifests/") {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("catalog %q", w.catalog.puts)
	}
	second := w.pushImage(t, "two")
	r = do(t, w.port("demo-docker"), "PUT", "/v2/app/manifests/1.0", second, "basic", asAdmin, "Content-Type", dockerManifestType)
	if r.status != http.StatusBadRequest || errorCodeOf(t, r.body) != "DENIED" {
		t.Fatalf("second push: %d %q", r.status, r.body)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-docker", "v2/app/manifests/"+sha256Digest(second)); row != nil {
		t.Fatalf("a refused tag left the digest blob %+v", row)
	}
}

func errorCodeOf(t *testing.T, body []byte) string {
	t.Helper()
	var doc struct {
		Errors []struct{ Code string } `json:"errors"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || len(doc.Errors) != 1 {
		t.Fatalf("error body %q: %v", body, err)
	}
	return doc.Errors[0].Code
}

// Review Focus 2 of the execution plan: a PATCH cut after half its length
// leaves no blob and no row; the session goes on at its offset before the
// chunk, and a first chunk leaves no work file at all.
func TestDockerPatchCutAfterHalf(t *testing.T) {
	w := startOCI(t, nil)
	addr := w.port("demo-docker")
	location := w.startUploadAt(t, asAdmin)
	chunk := bytes.Repeat([]byte("c"), 64*1024)
	head := func(extra string) string {
		return "PATCH " + location + " HTTP/1.1\r\nHost: " + addr + "\r\nAuthorization: " + basicHeader(asAdmin) +
			"\r\nContent-Length: " + strconv.Itoa(len(chunk)) + "\r\n" + extra + "\r\n"
	}
	status := func() *answer { return do(t, addr, "GET", location, nil, "basic", asAdmin) }
	rawSend(t, addr, head(""), chunk[:len(chunk)/2])
	eventually(t, "the end of the first cut chunk", func() bool { r := status(); return r.status == 204 && r.header.Get("Range") == "0-0" })
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files after a cut first chunk: %v", files)
	}
	if r := do(t, addr, "PATCH", location, chunk, "basic", asAdmin); r.status != http.StatusAccepted {
		t.Fatalf("PATCH: %d %q", r.status, r.body)
	}
	rawSend(t, addr, head("Content-Range: "+strconv.Itoa(len(chunk))+"-"+strconv.Itoa(2*len(chunk)-1)+"\r\n"), chunk[:len(chunk)/2])
	eventually(t, "the end of the second cut chunk", func() bool {
		files := w.workFiles()
		if len(files) != 1 {
			return false
		}
		info, err := os.Stat(filepath.Join(w.root(), "content", "tmp", files[0]))
		return err == nil && info.Size() == int64(len(chunk)) && status().header.Get("Range") == "0-"+strconv.Itoa(len(chunk)-1)
	})
	whole := append(append([]byte{}, chunk...), chunk...)
	r := do(t, addr, "PUT", location+digestQuery(whole), chunk, "basic", asAdmin, "Content-Range", strconv.Itoa(len(chunk))+"-"+strconv.Itoa(2*len(chunk)-1))
	if r.status != http.StatusCreated {
		t.Fatalf("PUT: %d %q", r.status, r.body)
	}
	if got := do(t, addr, "GET", "/v2/app/blobs/"+sha256Digest(whole), nil, "basic", asAdmin); !bytes.Equal(got.body, whole) {
		t.Fatalf("the blob has %d bytes", len(got.body))
	}
	if counts, _ := w.scan(); counts.Live != 1 || len(w.workFiles()) != 0 {
		t.Fatalf("scan %+v, work files %v", counts, w.workFiles())
	}
}

// The actions a write of an npm or a Docker path asks (ociAction): a
// dist-tag of an existing package asks edit, the rewrite of a document
// under its revision delete, the cancel of an upload add.
func TestOCIWriteActions(t *testing.T) {
	w := startOCI(t, nil)
	data := npmTarballOf(t, "package 1.0.0")
	if r := do(t, w.addr(), "PUT", "/repository/demo-npm/demo-pkg", npmPublishBody("demo-pkg", "1.0.0", data), "basic", asAdder); r.status != http.StatusCreated {
		t.Fatalf("a new package with add: %d %q", r.status, r.body)
	}
	for _, c := range []struct{ method, target, body string }{
		{"PUT", "/repository/demo-npm/-/package/demo-pkg/dist-tags/beta", `"1.0.0"`},
		{"DELETE", "/repository/demo-npm/-/package/demo-pkg/dist-tags/latest", ""},
		{"PUT", "/repository/demo-npm/demo-pkg/-rev/1-x", `{"versions": {}}`},
		{"PUT", "/repository/demo-npm/demo-pkg", string(npmPublishBody("demo-pkg", "1.1.0", data))},
	} {
		if r := do(t, w.addr(), c.method, c.target, []byte(c.body), "basic", asAdder); r.status != http.StatusForbidden {
			t.Errorf("%s %s with add only: %d %q", c.method, c.target, r.status, r.body)
		}
	}
	if r := do(t, w.addr(), "PUT", "/repository/demo-npm/-/package/demo-pkg/dist-tags/beta", []byte(`"1.0.0"`), "basic", asAdmin); r.status != http.StatusOK {
		t.Fatalf("dist-tag as admin: %d %q", r.status, r.body)
	}
	location := w.startUploadAt(t, asPusher)
	if r := do(t, w.port("demo-docker"), "DELETE", location, nil, "basic", asPusher); r.status != http.StatusNoContent {
		t.Fatalf("cancel with add only: %d %q", r.status, r.body)
	}
	if r := do(t, w.port("demo-docker"), "DELETE", "/v2/app/manifests/1.0", nil, "basic", asPusher); r.status != http.StatusForbidden {
		t.Fatalf("a manifest delete with add only: %d %q", r.status, r.body)
	}
}

// Open point 3e of leg 4d: an upload of a digest the repository holds
// already writes no second blob (task 13 of the phase 4 plan), and its bytes
// must still have that digest (checkHeldBlob): other bytes answer 400 with
// DIGEST_INVALID, as a first upload of them does (OCI distribution spec
// v1.1.1, section Pushing blobs, read 2026-11-18). Both for a monolithic
// POST and for the closing PUT of a session.
func TestDockerUploadOfAHeldDigestChecksTheBytes(t *testing.T) {
	w := startOCI(t, nil)
	layer := []byte("a layer the repository holds")
	if r := do(t, w.port("demo-docker"), "POST", "/v2/app/blobs/uploads/"+digestQuery(layer), layer, "basic", asAdmin); r.status != http.StatusCreated {
		t.Fatalf("the first upload: %d %q", r.status, r.body)
	}
	before, _ := w.scan()
	other := []byte("other bytes under the digest of the layer")
	if r := do(t, w.port("demo-docker"), "POST", "/v2/app/blobs/uploads/"+digestQuery(layer), other, "basic", asAdmin); r.status != http.StatusBadRequest ||
		errorCodeOf(t, r.body) != "DIGEST_INVALID" {
		t.Fatalf("a monolithic upload of other bytes: %d %q", r.status, r.body)
	}
	session := w.startUploadAt(t, asAdmin)
	if r := do(t, w.port("demo-docker"), "PATCH", session, other, "basic", asAdmin); r.status != http.StatusAccepted {
		t.Fatalf("PATCH: %d %q", r.status, r.body)
	}
	if r := do(t, w.port("demo-docker"), "PUT", session+digestQuery(layer), nil, "basic", asAdmin); r.status != http.StatusBadRequest ||
		errorCodeOf(t, r.body) != "DIGEST_INVALID" {
		t.Fatalf("the closing PUT of other bytes: %d %q", r.status, r.body)
	}
	if r := do(t, w.port("demo-docker"), "POST", "/v2/app/blobs/uploads/"+digestQuery(layer), layer, "basic", asAdmin); r.status != http.StatusCreated ||
		r.header.Get("Docker-Content-Digest") != sha256Digest(layer) {
		t.Fatalf("an upload of the same bytes: %d %v %q", r.status, r.header, r.body)
	}
	if after, _ := w.scan(); after != before {
		t.Fatalf("the uploads of a held digest wrote %+v, before %+v", after, before)
	}
	if got := do(t, w.port("demo-docker"), "GET", "/v2/app/blobs/"+sha256Digest(layer), nil, "basic", asAdmin); string(got.body) != string(layer) {
		t.Fatalf("%d %q", got.status, got.body)
	}
	eventually(t, "no work file", func() bool { return len(w.workFiles()) == 0 })
}
