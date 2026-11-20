package server

import (
	"net/http"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Task 12 of the phase 5 plan, maven2 (decision 5.10): after a new artifact
// a proxy fetches the .sha1 of that artifact from its remote once and
// compares. A wrong .sha1 answers 502 and stores nothing; a remote without
// the file is accepted; a .sha1 the remote fails to give fails the fetch. An
// artifact is a maven2 path that is neither metadata nor a checksum file
// itself: maven-metadata.xml, its checksums and every checksum file a client
// asks for are fetched as they are, without a .sha1 request.

// mavenProxy is the remote folder and the proxy of the maven2 tests.
const mavenProxy = "demo-maven-proxy"

func TestProxyMavenComparesTheSha1OfANewArtifact(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: fetchProxies})
	base := remoteBase(mavenProxy)
	const (
		right    = "com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar"
		wrong    = "com/example/demo-lib/1.1.0/demo-lib-1.1.0.jar"
		without  = "com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar"
		failing  = "com/example/demo-lib/1.3.0/demo-lib-1.3.0.jar"
		named    = "com/example/demo-lib/1.4.0/demo-lib-1.4.0.pom"
		metadata = "com/example/demo-lib/maven-metadata.xml"
		checksum = "com/example/demo-lib/1.5.0/demo-lib-1.5.0.jar.md5"
	)
	content := func(path string) string { return "the bytes of " + path + "\n" }
	u.remote.Script(base+right, testkit.OK(content(right)))
	u.remote.Script(base+right+".sha1", testkit.OK(sha1Of(content(right))))
	u.remote.Script(base+wrong, testkit.OK(content(wrong)))
	u.remote.Script(base+wrong+".sha1", testkit.OK(sha1Of("other bytes")))
	u.remote.Script(base+without, testkit.OK(content(without)))
	u.remote.Script(base+failing, testkit.OK(content(failing)))
	u.remote.Script(base+failing+".sha1", testkit.ServerError())
	u.remote.Script(base+named, testkit.OK(content(named)))
	u.remote.Script(base+named+".sha1", testkit.OK(sha1Of(content(named))+"  demo-lib-1.4.0.pom\n"))
	u.remote.Script(base+metadata, testkit.OK("<metadata/>\n"))
	u.remote.Script(base+checksum, testkit.OK("d41d8cd98f00b204e9800998ecf8427e\n"))
	for _, c := range []struct {
		path   string
		status int
		stored bool
		sha1s  int // requests of the .sha1 of the path at the remote
	}{
		{right, 200, true, 1},
		{wrong, http.StatusBadGateway, false, 1},
		{without, 200, true, 1},
		{failing, http.StatusBadGateway, false, 1},
		{named, 200, true, 1},
		{metadata, 200, true, 0},
		{checksum, 200, true, 0},
	} {
		r := u.get(mavenProxy, c.path)
		if r.status != c.status || c.status == 200 && string(r.body) != content(c.path) && c.path != metadata && c.path != checksum {
			t.Errorf("%s: %d %q, want %d", c.path, r.status, r.body, c.status)
		}
		if row := u.row(mavenProxy, c.path); (row != nil) != c.stored {
			t.Errorf("%s: row %+v, want stored %v", c.path, row, c.stored)
		}
		if n := u.remote.Count(base + c.path); n != 1 {
			t.Errorf("%s: %d requests at the remote, want 1", c.path, n)
		}
		if n := u.remote.Count(base + c.path + ".sha1"); n != c.sha1s {
			t.Errorf("%s: %d requests of its .sha1 at the remote, want %d", c.path, n, c.sha1s)
		}
	}
	// 5 files are stored, each a bytes and a properties file; the refused
	// downloads left nothing, also no work file
	if blobs, work := u.blobFiles(); len(blobs) != 10 || len(work) != 0 {
		t.Fatalf("blobs %v, work files %v", blobs, work)
	}
	// a cached artifact and its .sha1 answer from the cache: no request
	u.remote.Reset()
	if r := u.get(mavenProxy, right); r.status != 200 || string(r.body) != content(right) {
		t.Fatalf("again: %d %q", r.status, r.body)
	}
	if r := u.get(mavenProxy, right+".sha1"); r.status != 200 || string(r.body) != sha1Of(content(right)) {
		t.Fatalf("the .sha1 of a cached artifact: %d %q", r.status, r.body)
	}
	if n := len(u.remote.Requests()); n != 0 {
		t.Fatalf("%d requests at the remote for cached paths: %+v", n, u.remote.Requests())
	}
}

// A stale artifact the remote confirms with 304 asks for no .sha1; new bytes
// of a stale artifact are new, and their .sha1 is asked once more.
func TestProxyMavenAsksTheSha1OfNewBytesOnly(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: mavenProxy, format: "maven2",
		attributes: map[string]any{"proxy": map[string]any{"contentMaxAge": 0}}}}})
	base := remoteBase(mavenProxy)
	const jar = "com/example/demo-lib/2.0.0/demo-lib-2.0.0.jar"
	first, second := "the first bytes\n", "the second bytes\n"
	u.remote.Script(base+jar, testkit.OK(first).With("ETag", `"one"`), testkit.NotModified(), testkit.OK(second).With("ETag", `"two"`))
	u.remote.Script(base+jar+".sha1", testkit.OK(sha1Of(first)), testkit.OK(sha1Of(second)))
	for i, want := range []string{first, first, second} {
		if r := u.get(mavenProxy, jar); r.status != 200 || string(r.body) != want {
			t.Fatalf("request %d: %d %q, want %q", i+1, r.status, r.body, want)
		}
	}
	if n := u.remote.Count(base + jar); n != 3 {
		t.Fatalf("%d requests of the jar, want 3", n)
	}
	if n := u.remote.Count(base + jar + ".sha1"); n != 2 {
		t.Fatalf("%d requests of its .sha1, want 2: one per new bytes", n)
	}
}
