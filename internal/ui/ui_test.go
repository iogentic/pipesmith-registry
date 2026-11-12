package ui

import (
	"bytes"
	"crypto/sha1" // #nosec G505 -- git object ids are SHA-1; the test recomputes the tree id of the taken commit.
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// treeID is the git tree id of ui/web of the interface repository at commit
// 1e6d884a06f44de2a3c42b5ce91476b8540a80d0, read with git rev-parse
// 1e6d884:ui/web on 2026-11-12.
const treeID = "43014e95e7dcd77ef6e6e1552a6b3d2dce35b638"

const policy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// gitTree is the id git gives the directory dir of fsys: blobs and trees
// in git's order, where a directory sorts as its name with a slash.
func gitTree(t *testing.T, fsys fs.FS, dir string) []byte {
	t.Helper()
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool {
		key := func(e fs.DirEntry) string {
			if e.IsDir() {
				return e.Name() + "/"
			}
			return e.Name()
		}
		return key(entries[i]) < key(entries[j])
	})
	var body bytes.Buffer
	for _, e := range entries {
		full := path.Join(dir, e.Name())
		if e.IsDir() {
			fmt.Fprintf(&body, "40000 %s\x00", e.Name())
			body.Write(gitTree(t, fsys, full))
			continue
		}
		data, err := fs.ReadFile(fsys, full)
		if err != nil {
			t.Fatal(err)
		}
		blob := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...)) // #nosec G401 -- git object id.
		fmt.Fprintf(&body, "100644 %s\x00", e.Name())
		body.Write(blob[:])
	}
	tree := sha1.Sum(append([]byte(fmt.Sprintf("tree %d\x00", body.Len())), body.Bytes()...)) // #nosec G401 -- git object id.
	return tree[:]
}

func TestEmbeddedTreeIsTheTakenCommit(t *testing.T) {
	web, err := fs.Sub(files, "web")
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(gitTree(t, web, ".")); got != treeID {
		t.Fatalf("tree id of the embedded files %s, want %s: internal/ui/web differs from the taken commit", got, treeID)
	}
}

func serve(t *testing.T, h *Handler, method, target string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func mustNew(t *testing.T) *Handler {
	t.Helper()
	h, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func checkHeaders(t *testing.T, w *httptest.ResponseRecorder, contentType string) {
	t.Helper()
	want := map[string]string{
		"Content-Security-Policy": policy,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
		"Content-Type":            contentType,
		"Cache-Control":           "no-cache",
	}
	for key, value := range want {
		if got := w.Header().Get(key); got != value {
			t.Errorf("%s: %q, want %q", key, got, value)
		}
	}
	if !regexp.MustCompile(`^"[0-9a-f]{32}"$`).MatchString(w.Header().Get("ETag")) {
		t.Errorf("ETag %q", w.Header().Get("ETag"))
	}
}

func TestRootServesTheIndexPage(t *testing.T) {
	h := mustNew(t)
	page, err := fs.ReadFile(files, "web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	w := serve(t, h, "GET", "/")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), page) {
		t.Fatalf("GET /: %d, %d bytes, want 200 and index.html (%d bytes)", w.Code, w.Body.Len(), len(page))
	}
	checkHeaders(t, w, "text/html; charset=utf-8")
	if w.Header().Get("Content-Length") != fmt.Sprint(len(page)) {
		t.Errorf("Content-Length %q", w.Header().Get("Content-Length"))
	}
}

func TestEveryFileIsServedAtItsPath(t *testing.T) {
	h := mustNew(t)
	types := map[string]string{".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8",
		".png": "image/png", ".html": "text/html; charset=utf-8"}
	served := 0
	err := fs.WalkDir(files, "web", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || name == "web/index.html" {
			return err
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		target := strings.TrimPrefix(name, "web")
		w := serve(t, h, "GET", target)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
			t.Errorf("GET %s: %d, %d bytes, want 200 and %d bytes", target, w.Code, w.Body.Len(), len(data))
		}
		checkHeaders(t, w, types[path.Ext(name)])
		head := serve(t, h, "HEAD", target)
		if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != fmt.Sprint(len(data)) {
			t.Errorf("HEAD %s: %d, body %d bytes, Content-Length %q", target, head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
		}
		served++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 70 files at the taken commit, counted with git ls-tree on 2026-11-12; index.html is served at "/"
	if served != 69 {
		t.Fatalf("%d files served below /css/, /js/ and /assets/, want 69", served)
	}
}

func TestOnlyThePageAndItsThreeFoldersAreServed(t *testing.T) {
	h := mustNew(t)
	for _, target := range []string{"/index.html", "/css/", "/js", "/js/", "/assets/", "/css/../index.html", "/js/missing.js",
		"/mock/server.py", "/web/index.html", "/favicon.ico", "/CSS/base.css", "/css/base.css/"} {
		if h.Has(target) {
			t.Errorf("Has(%q) is true", target)
		}
		if w := serve(t, h, "GET", target); w.Code != 404 {
			t.Errorf("GET %s: %d, want 404", target, w.Code)
		}
	}
	for _, target := range []string{"/", "/css/base.css", "/js/app.js", "/js/pages/browse/tree.js", "/assets/favicon-32.png"} {
		if !h.Has(target) {
			t.Errorf("Has(%q) is false", target)
		}
	}
}

func TestOtherMethodsAreRefused(t *testing.T) {
	h := mustNew(t)
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		w := serve(t, h, method, "/js/app.js")
		if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d, Allow %q", method, w.Code, w.Header().Get("Allow"))
		}
	}
}

func TestEntityTagGivesNotModified(t *testing.T) {
	h := mustNew(t)
	first := serve(t, h, "GET", "/css/base.css")
	tag := first.Header().Get("ETag")
	again := serve(t, h, "GET", "/css/base.css", "If-None-Match", tag)
	if again.Code != 304 || again.Body.Len() != 0 || again.Header().Get("ETag") != tag {
		t.Fatalf("If-None-Match: %d, %d bytes, ETag %q", again.Code, again.Body.Len(), again.Header().Get("ETag"))
	}
	other := serve(t, h, "GET", "/css/base.css", "If-None-Match", `"0000"`)
	if other.Code != 200 {
		t.Fatalf("another tag: %d", other.Code)
	}
	if serve(t, h, "GET", "/js/app.js").Header().Get("ETag") == tag {
		t.Fatal("two files with one tag")
	}
}

// The policy forbids inline script and style, so no taken file may need one.
func TestNoInlineScriptOrStyle(t *testing.T) {
	inline := regexp.MustCompile(`(?i)<script(\s[^>]*)?>\s*[^<\s]|<style|\sstyle=|\son[a-z]+=`)
	err := fs.WalkDir(files, "web", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(name) != ".html" {
			return err
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		if m := inline.Find(data); m != nil {
			t.Errorf("%s holds %q", name, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestServesThroughAnyResponseWriter(t *testing.T) {
	h := mustNew(t)
	var _ http.Handler = h
	w := serve(t, h, "GET", "/assets/favicon-32.png")
	if w.Code != 200 || !bytes.HasPrefix(w.Body.Bytes(), []byte("\x89PNG")) {
		t.Fatalf("favicon: %d %q", w.Code, w.Body.Bytes()[:min(8, w.Body.Len())])
	}
}
