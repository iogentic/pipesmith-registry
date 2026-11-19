package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Open point 1 of the 4c report: npm dist-tag add, rm and ls read GET
// /-/package/<name>/dist-tags before they write (npm/cli, branch latest,
// lib/commands/dist-tag.js, fetchTags, read 2026-11-18); the answer is the
// dist-tags object of the package, an empty one a failure of the client.
// With writes on it is served for a hosted repository and a group; with
// writes off every path below "-" but ping keeps the npm 404 of 0.1.1.
func TestNpmDistTagsAreServed(t *testing.T) {
	w := startREST(t, restAttributes())
	for _, name := range []string{"demo-tags", "@demo/tags"} {
		for _, version := range []string{"1.0.0", "2.0.0"} {
			status(t, w.upload("demo-npm", asAdmin, formFile("npm.asset", "p.tgz", packageTarball(t, name, version))), 204)
		}
	}
	escaped := map[string]string{"demo-tags": "demo-tags", "@demo/tags": "@demo%2ftags"}
	for name, path := range escaped {
		for _, repo := range []string{"demo-npm", "demo-npm-group"} {
			r := do(t, w.addr(), "GET", "/repository/"+repo+"/-/package/"+path+"/dist-tags", nil, "basic", asAdmin)
			var tags map[string]string
			if r.status != 200 || r.header.Get("Content-Type") != "application/json" || json.Unmarshal(r.body, &tags) != nil ||
				len(tags) != 1 || tags["latest"] != "2.0.0" {
				t.Fatalf("%s of %s: %d %v %q", name, repo, r.status, r.header, r.body)
			}
		}
	}
	for _, target := range []string{"/repository/demo-npm/-/package/demo-none/dist-tags", "/repository/demo-npm/-/package/demo-tags/dist-tags/latest",
		"/repository/demo-npm/-/package/demo-tags", "/repository/demo-npm/-/package/demo-tags/dist-tags/"} {
		if r := do(t, w.addr(), "GET", target, nil, "basic", asAdmin); r.status != 404 || string(r.body) != npmNotFoundBody {
			t.Errorf("%s: %d %q", target, r.status, r.body)
		}
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-npm/-/package/demo-tags/dist-tags", nil, "basic", asNobody); r.status != 403 {
		t.Fatalf("without read: %d", r.status)
	}
	off := startWriting(t, newFakeSecurity(), false)
	if r := do(t, off.addr(), "GET", "/repository/demo-npm/-/package/widget/dist-tags", nil, "basic", asAdmin); r.status != 404 ||
		string(r.body) != npmNotFoundBody {
		t.Fatalf("writes off: %d %q", r.status, r.body)
	}
}

// Open point 2 of the 4c report: a publish whose document fails after its
// tarball is stored leaves no live tarball. The tarball gets the delete
// mark and loses its row, so that the read path does not build the version
// from it, and a second publish of the same version is taken, under
// ALLOW_ONCE too. The document of a publish carries a readme that the limit
// of the repository refuses, the tarball passes it.
func TestNpmPublishWhoseDocumentFailsLeavesNoTarball(t *testing.T) {
	for _, policy := range []string{"ALLOW_ONCE", "ALLOW"} {
		t.Run(policy, func(t *testing.T) {
			w := startOCI(t, map[string]map[string]any{"demo-npm": {"storage": map[string]any{"writePolicy": policy}, "max_body_bytes": 2048}})
			data := npmTarballOf(t, "a small tarball")
			var big map[string]any
			if err := json.Unmarshal(npmPublishBody("demo-fail", "1.0.0", data), &big); err != nil {
				t.Fatal(err)
			}
			big["readme"] = strings.Repeat("a readme the document cannot hold ", 200)
			body, _ := json.Marshal(big)
			if r := do(t, w.addr(), "PUT", "/repository/demo-npm/demo-fail", body, "basic", asAdmin); r.status != 413 {
				t.Fatalf("the publish of a document above the limit: %d %q", r.status, r.body)
			}
			const tarballPath = "demo-fail/-/demo-fail-1.0.0.tgz"
			if row, _ := w.srv.rp.index.Lookup("demo-npm", tarballPath); row != nil {
				t.Fatalf("the tarball keeps its row: %+v", row)
			}
			_, live := w.scan()
			for _, e := range live {
				if e.Path == tarballPath {
					t.Fatalf("the tarball is a live blob: %+v", e)
				}
			}
			if r := do(t, w.addr(), "GET", "/repository/demo-npm/demo-fail", nil, "basic", asAdmin); r.status != 404 {
				t.Fatalf("the read path serves the version: %d %q", r.status, r.body)
			}
			r := do(t, w.addr(), "PUT", "/repository/demo-npm/demo-fail", npmPublishBody("demo-fail", "1.0.0", data), "basic", asAdmin)
			if r.status != http.StatusCreated {
				t.Fatalf("the second publish: %d %q", r.status, r.body)
			}
			if got := do(t, w.addr(), "GET", "/repository/demo-npm/"+tarballPath, nil, "basic", asAdmin); !bytes.Equal(got.body, data) {
				t.Fatalf("%d %q", got.status, got.body)
			}
		})
	}
}
