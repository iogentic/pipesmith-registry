package server

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// A write takes a repository that is hosted and online by its stored record
// too (finding N3 of review 4). demo-proxy has no definition in
// repositories.json: the index discovers it from a blob its cache holds, as
// a hosted maven2 repository, and its record says proxy with a write policy.
// demo-offline is hosted, and its record says offline. Neither takes a
// write, by the client route, by the REST upload or by the rebuild of the
// Maven metadata; demo-raw does.
func TestAWriteTargetIsHostedAndOnlineByItsRecord(t *testing.T) {
	sec := newFakeSecurity()
	snap := *sec.snap
	snap.Repositories = []model.Repository{
		{Name: "demo-raw", Format: "raw", Type: "hosted", Online: true, Attributes: storage("ALLOW")},
		{Name: "demo-proxy", Format: "maven2", Type: "proxy", Online: true, Attributes: storage("ALLOW")},
		{Name: "demo-offline", Format: "raw", Type: "hosted", Online: false, Attributes: storage("ALLOW")},
	}
	sec.snap = &snap
	base := t.TempDir()
	cached := spec("demo-proxy", "com/example/cached/1.0/cached-1.0.pom", []byte("<project/>"), "application/xml")
	storeCfgs := []config.StoreConfig{blobStore(t, base, "store-a", cached)}
	opts := appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted"), def("demo-offline", "raw", "hosted")}, security: sec, writes: true}
	a := startApp(t, base, prepare(t, base, storeCfgs, opts), opts)
	for _, c := range []struct {
		repo, path  string
		put, upload int
	}{
		{"demo-raw", "docs/a.txt", http.StatusCreated, http.StatusNoContent},
		{"demo-proxy", "com/example/cached/1.1/cached-1.1.pom", http.StatusMethodNotAllowed, http.StatusBadRequest},
		{"demo-offline", "docs/a.txt", http.StatusMethodNotAllowed, http.StatusBadRequest},
	} {
		if r := do(t, a.addr(), "PUT", "/repository/"+c.repo+"/"+c.path, []byte("<project/>"), "basic", asAdmin); r.status != c.put {
			t.Errorf("PUT to %s: %d %q", c.repo, r.status, r.body)
		}
		if r := do(t, a.addr(), "DELETE", "/repository/"+c.repo+"/"+c.path, nil, "basic", asAdmin); c.put != http.StatusCreated &&
			r.status != http.StatusMethodNotAllowed {
			t.Errorf("DELETE in %s: %d %q", c.repo, r.status, r.body)
		}
		form, contentType := multipartBody(t, formField("raw.directory", "up"), formFile("raw.asset1", "b.txt", []byte("b")),
			formField("raw.asset1.filename", "b.txt"))
		if c.repo == "demo-proxy" {
			form, contentType = multipartBody(t, formField("maven2.groupId", "com.example"), formField("maven2.artifactId", "cached"),
				formField("maven2.version", "1.2"), formFile("maven2.asset1", "cached.jar", []byte("PK")), formField("maven2.asset1.extension", "jar"))
		}
		r := do(t, a.addr(), "POST", componentsPath+"?repository="+url.QueryEscape(c.repo), form, "basic", asAdmin, "Content-Type", contentType)
		if r.status != c.upload {
			t.Errorf("upload to %s: %d %q", c.repo, r.status, r.body)
		}
	}
	if _, err := a.srv.RebuildMavenMetadata("demo-proxy", maven.Scope{}, nil, ""); err == nil {
		t.Error("the metadata of demo-proxy is rebuilt")
	}
}
