package server

import (
	"path/filepath"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/config"
	statefile "github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Finding of 2026-11-19: a stored HTML or SVG file was answered with its
// stored type and no other header, on the origin of the interface, so its
// script ran with the session of whoever opened its link. Every answer that
// carries the bytes of a stored file now says X-Content-Type-Options:
// nosniff and Content-Security-Policy: sandbox, which runs no script and
// gives the document an origin of its own.
//
// The old product sends a sandbox policy on repository content too: its
// security advisory of 4 December 2025 names a regression of its versions
// 3.83.0 to 3.86.2 in which the header was missing from some uploaded
// content, and as the interim fix a reverse proxy that adds a
// Content-Security-Policy sandbox header to every answer below
// /repository/. Read on 2026-11-19 in a search summary, since the page
// refuses automated reads; no page read gives the exact value it sends. A
// forum post of 6 May 2020 by a user adds allow-same-origin to the value
// "sandbox allow-forms allow-modals allow-popups allow-presentation
// allow-scripts allow-top-navigation"; plain sandbox allows none of these.

const (
	storedHTML = `<html><body><script>fetch("/service/rest/v1/security/users")</script></body></html>`
	storedSVG  = `<svg xmlns="http://www.w3.org/2000/svg"><script>fetch("/service/rest/v1/security/users")</script></svg>`
)

func sandboxed(t *testing.T, what string, r *response, status int) {
	t.Helper()
	if r.status != status || r.header.Get("X-Content-Type-Options") != "nosniff" || r.header.Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("%s: %d, X-Content-Type-Options %q, Content-Security-Policy %q, want %d, nosniff, sandbox", what, r.status,
			r.header.Get("X-Content-Type-Options"), r.header.Get("Content-Security-Policy"), status)
	}
}

// sandboxApp serves stored HTML and SVG files in every format, with security
// on as a state.sqlite turns it on, or off.
func sandboxApp(t *testing.T, secure bool) *app {
	t.Helper()
	base := t.TempDir()
	blobs := []testkit.BlobSpec{
		spec("demo-raw", "site/index.html", []byte(storedHTML), "text/html"),
		spec("demo-raw", "site/logo.svg", []byte(storedSVG), "image/svg+xml"),
		spec("demo-releases", "com/example/demo/1.0/demo-1.0.html", []byte(storedHTML), "text/html"),
		spec("demo-npm", "demo-lib", []byte(`{"name": "demo-lib", "versions": {"1.0.0": {"name": "demo-lib", "version": "1.0.0"}}}`),
			"application/json"),
		spec("demo-npm", "demo-lib/-/demo-lib-1.0.0.tgz", []byte(storedHTML), "text/html"),
	}
	blobs = append(blobs, blobsOf("demo-docker", dockerImage("app", "1.0", []byte(storedHTML)))...)
	opts := appOptions{defs: append(groupDefs(), def("demo-raw", "raw", "hosted"), def("demo-npm", "npm", "hosted"),
		dockerDef("demo-docker", 14692)), dockerRepos: []string{"demo-docker"}}
	if secure {
		st, err := statefile.Create(filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		snap := tokenSnapshot(t)
		if err := st.Write(statefile.Kinds, snap); err != nil {
			t.Fatal(err)
		}
		if opts.security, err = auth.New(auth.Options{Snapshot: snap, Store: st}); err != nil {
			t.Fatal(err)
		}
	}
	return newApp(t, []config.StoreConfig{blobStore(t, base, "s1", blobs...)}, opts)
}

func TestStoredFilesAreSandboxed(t *testing.T) {
	a := sandboxApp(t, true)
	as := func(headers ...string) []string {
		return append([]string{"Authorization", basicPair(adminUser, adminPass)}, headers...)
	}
	file := "/repository/demo-raw/site/index.html"
	sandboxed(t, "raw HTML", a.get(file, as()...), 200)
	sandboxed(t, "raw SVG", a.get("/repository/demo-raw/site/logo.svg", as()...), 200)
	sandboxed(t, "HEAD", a.head(file, as()...), 200)
	sandboxed(t, "range", a.get(file, as("Range", "bytes=0-5")...), 206)
	sandboxed(t, "range not satisfiable", a.get(file, as("Range", "bytes=9999-")...), 416)
	etag := a.get(file, as()...).header.Get("ETag")
	sandboxed(t, "not modified", a.get(file, as("If-None-Match", etag)...), 304)
	sandboxed(t, "maven file", a.get("/repository/demo-releases/com/example/demo/1.0/demo-1.0.html", as()...), 200)
	sandboxed(t, "group member", a.get("/repository/demo-group/com/example/demo/1.0/demo-1.0.html", as()...), 200)
	sandboxed(t, "npm tarball", a.get("/repository/demo-npm/demo-lib/-/demo-lib-1.0.0.tgz", as()...), 200)
	sandboxed(t, "npm package document", a.get("/repository/demo-npm/demo-lib", as("Host", "localhost:14193")...), 200)
	layer := digestOf([]byte(storedHTML))
	sandboxed(t, "Docker blob", a.on("demo-docker", "GET", "/v2/app/blobs/"+layer, as()...), 200)
	sandboxed(t, "Docker manifest", a.on("demo-docker", "GET", "/v2/app/manifests/1.0", as()...), 200)
	sandboxed(t, "Docker blob on the main port", a.get("/repository/demo-docker/v2/app/blobs/"+layer, as()...), 200)

	// answers that carry no stored file keep their headers: the listing of
	// a directory, a refusal, a missing file
	for _, c := range []struct {
		what, target string
		headers      []string
		status       int
	}{
		{"listing", "/repository/demo-raw/site/", as(), 200},
		{"without credentials", file, nil, 401},
		{"missing file", "/repository/demo-raw/site/none.html", as(), 404},
	} {
		if r := a.get(c.target, c.headers...); r.status != c.status || r.header.Get("Content-Security-Policy") != "" {
			t.Errorf("%s: %d, Content-Security-Policy %q", c.what, r.status, r.header.Get("Content-Security-Policy"))
		}
	}
	// without security there is no session to take: the answer stays the
	// one of step 2, as the format vectors pin it
	if r := sandboxApp(t, false).get(file); r.status != 200 || r.header.Get("Content-Security-Policy") != "" ||
		r.header.Get("X-Content-Type-Options") != "" {
		t.Errorf("security off: %d %v", r.status, r.header)
	}
}
