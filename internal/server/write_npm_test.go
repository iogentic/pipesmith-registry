package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1" // #nosec G505 -- the shasum of an npm version, as the client gives it.
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The npm writes of leg 4c (task 11 of the phase 4 plan) through the real
// handler, with the fake security of secure_test.go. The black box proofs
// are tests/write/test_npm.py.

// npmTarballOf is a gzip stream that stands for a tarball.
func npmTarballOf(t *testing.T, text string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	if _, err := z.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// npmPublishBody is the document npm publish sends for name@version.
func npmPublishBody(name, version string, data []byte) []byte {
	sum := sha1.Sum(data) // #nosec G401 -- see the import.
	unscoped := name[strings.LastIndex(name, "/")+1:]
	doc := map[string]any{"_id": name, "name": name, "dist-tags": map[string]any{"latest": version},
		"versions": map[string]any{version: map[string]any{"name": name, "version": version,
			"dist": map[string]any{"shasum": hex.EncodeToString(sum[:]), "tarball": "http://elsewhere/" + name + "/-/" + unscoped + "-" + version + ".tgz"}}},
		"_attachments": map[string]any{unscoped + "-" + version + ".tgz": map[string]any{"content_type": "application/octet-stream",
			"data": base64.StdEncoding.EncodeToString(data), "length": len(data)}}}
	out, _ := json.Marshal(doc)
	return out
}

// A publish writes the tarball and the merged document as two blobs with
// their rows and catalog rows (W10); a second version merges into the
// document; the read path serves both.
func TestNpmPublishIsTwoBlobs(t *testing.T) {
	w := startOCI(t, nil)
	for _, version := range []string{"1.0.0", "1.1.0"} {
		data := npmTarballOf(t, "package "+version)
		r := do(t, w.addr(), "PUT", "/repository/demo-npm/@demo%2fpkg", npmPublishBody("@demo/pkg", version, data), "basic", asAdmin)
		if r.status != http.StatusCreated || !strings.Contains(string(r.body), `"ok":true`) {
			t.Fatalf("publish %s: %d %q", version, r.status, r.body)
		}
		tarball, _ := w.srv.rp.index.Lookup("demo-npm", "@demo/pkg/-/pkg-"+version+".tgz")
		if tarball == nil || tarball.Store != "local-write" || *tarball.ContentType != "application/gzip" {
			t.Fatalf("tarball row %+v", tarball)
		}
	}
	doc, _ := w.srv.rp.index.Lookup("demo-npm", "@demo/pkg")
	if doc == nil || *doc.ContentType != "application/json" {
		t.Fatalf("document row %+v", doc)
	}
	if props := w.properties(doc.Location); props["@BlobStore.blob-name"] != "/@demo/pkg" || props["@Bucket.repo-name"] != "demo-npm" {
		t.Fatalf("%q", props)
	}
	served := do(t, w.addr(), "GET", "/repository/demo-npm/@demo%2fpkg", nil, "basic", asAdmin).object(t)
	versions, _ := served["versions"].(map[string]any)
	tags, _ := served["dist-tags"].(map[string]any)
	if len(versions) != 2 || tags["latest"] != "1.1.0" || !strings.HasPrefix(served["_rev"].(string), "2-") {
		t.Fatalf("%v", served)
	}
	puts := strings.Join(w.catalog.puts, "\n")
	for _, path := range []string{"demo-npm/@demo/pkg/-/pkg-1.0.0.tgz", "demo-npm/@demo/pkg/-/pkg-1.1.0.tgz", "demo-npm/@demo/pkg "} {
		if !strings.Contains(puts, path) {
			t.Fatalf("catalog %q lacks %s", puts, path)
		}
	}
	if len(w.workFiles()) != 0 {
		t.Fatalf("work files %v", w.workFiles())
	}
}
