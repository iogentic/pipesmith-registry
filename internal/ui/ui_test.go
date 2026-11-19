package ui

import (
	"bytes"
	"crypto/sha1" // #nosec G505 -- git object ids are SHA-1; the test recomputes the tree id of the taken commit.
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// treeID is the git tree id of ui/web of the interface repository at commit
// 1e6d884a06f44de2a3c42b5ce91476b8540a80d0, read with git rev-parse
// 1e6d884:ui/web on 2026-11-12.
const treeID = "43014e95e7dcd77ef6e6e1552a6b3d2dce35b638"

// changed are the files of web that differ from the taken commit, as
// LOCAL_CHANGES.md lists them: the git blob id each had at 1e6d884 (git
// rev-parse 1e6d884:ui/web/<file>) and the one it has here (git
// hash-object), both read on 2026-11-18; the id of js/app.js here read
// again on 2026-11-19, the ids of js/pages/signin.js read on 2026-11-19.
var changed = map[string]struct{ taken, here string }{
	"css/tokens.css":          {taken: "7be33c7811f56994bdbac5a66dabe7a96cf7ad55", here: "e0ae07e4bf4350534a436fa133117ab0ffbf74c8"},
	"js/app.js":               {taken: "4562a6861333f633f1375aa0105c98f23231833d", here: "efcb289c4a877beba54a0c57991b637a1e1b5435"},
	"js/pages/overview.js":    {taken: "121d922eb60a8338fcea601bd02919017a5f4f2c", here: "a81ca7d55d91b206894c79e43a913a4bc8b73973"},
	"js/pages/signin.js":      {taken: "7f980fb5d625dcad5eaa479be7e0702755262a8b", here: "4f56705ed045fe1f339b3f2ddbf002ff1d4610ce"},
	"js/preview.js":           {taken: "20675502d1edc6305e4529f80513279dd10b2075", here: "7f8738e53e9d79154450b08029992a61d83e99e2"},
	"js/api.js":               {taken: "6c512c140771011137ef5ad3579e553f9e943e68", here: "97039bf9cc360b0523614620c3fc036e28fab351"},
	"index.html":              {taken: "d2ac82b9bb632d5e7e1172f0e3033c2be9b5d541", here: "48ab43ce95387b7212307ac0576832c5a1f541c0"},
	"js/routes.js":            {taken: "a3e1dd35ec8b2bb404a59d54407d3f3b3a5e2e2c", here: "dd565d85abff79d05b46033100ac2b659ab5c7ad"},
	"js/pages/browse/tree.js": {taken: "cb56f5f1c081d4793a548d3432956249e30dd951", here: "7ce59159f30fffa53002ced4f9009917f6f7284b"},
}

// added are the files of web that the taken commit does not hold, as section Added Files of
// LOCAL_CHANGES.md lists them. The tree test leaves them out, and with them every folder that holds
// nothing else, so the rest of the tree is still compared with the taken tree.
var added = []string{
	"css/areas/viewer.css",
	"js/pages/browse/export-dialog.js",
	"js/pages/browse/view.js",
	"js/viewer/filetree.js",
	"js/viewer/grammar/asciidoc.js",
	"js/viewer/grammar/json.js",
	"js/viewer/grammar/keyvalue.js",
	"js/viewer/grammar/lines.js",
	"js/viewer/grammar/markdown.js",
	"js/viewer/grammar/result.js",
	"js/viewer/grammar/toml.js",
	"js/viewer/grammar/xml.js",
	"js/viewer/grammar/yaml.js",
	"js/viewer/kinds.js",
	"js/viewer/load.js",
	"js/viewer/model.js",
	"js/viewer/view.js",
}

const policy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// gitTree is the id git gives the directory dir of fsys: blobs and trees
// in git's order, where a directory sorts as its name with a slash. blob
// gives the id the tree records for a file, from its path below fsys and
// the id of its content.
func gitTree(t *testing.T, fsys fs.FS, dir string, blob func(name string, id []byte) []byte, skip func(name string) bool) []byte {
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
		if skip(full) {
			continue
		}
		if e.IsDir() {
			sub := gitTree(t, fsys, full, blob, skip)
			if sub == nil {
				continue
			}
			fmt.Fprintf(&body, "40000 %s\x00", e.Name())
			body.Write(sub)
			continue
		}
		data, err := fs.ReadFile(fsys, full)
		if err != nil {
			t.Fatal(err)
		}
		id := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...)) // #nosec G401 -- git object id.
		fmt.Fprintf(&body, "100644 %s\x00", e.Name())
		body.Write(blob(full, id[:]))
	}
	if body.Len() == 0 && dir != "." {
		return nil // a folder with nothing but skipped files is not in the tree
	}
	tree := sha1.Sum(append([]byte(fmt.Sprintf("tree %d\x00", body.Len())), body.Bytes()...)) // #nosec G401 -- git object id.
	return tree[:]
}

// The embedded files are the taken commit, apart from the files of changed:
// each holds the content its blob id here names, and with the taken blob put
// back for each of them the tree is the taken tree.
func TestEmbeddedFilesAreTheTakenCommitAndTheListedChanges(t *testing.T) {
	web, err := fs.Sub(files, "web")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	asTaken := func(name string, id []byte) []byte {
		file, ok := changed[name]
		if !ok {
			return id
		}
		seen[name] = true
		if got := hex.EncodeToString(id); got != file.here {
			t.Errorf("%s has the blob id %s, the list names %s: record the change in changed and in LOCAL_CHANGES.md", name, got, file.here)
		}
		taken, err := hex.DecodeString(file.taken)
		if err != nil {
			t.Fatal(err)
		}
		return taken
	}
	isAdded := map[string]bool{}
	for _, name := range added {
		isAdded[name] = true
	}
	skipped := map[string]bool{}
	skip := func(name string) bool {
		if isAdded[name] {
			skipped[name] = true
			return true
		}
		return false
	}
	if got := hex.EncodeToString(gitTree(t, web, ".", asTaken, skip)); got != treeID {
		t.Fatalf("tree id of the embedded files with the listed files as taken %s, want %s: a file of internal/ui/web differs from the taken commit and is not listed", got, treeID)
	}
	notes, err := os.ReadFile("LOCAL_CHANGES.md")
	if err != nil {
		t.Fatal(err)
	}
	for name, file := range changed {
		if !seen[name] {
			t.Errorf("%s is listed as changed but not embedded", name)
		}
		if file.taken == file.here {
			t.Errorf("%s is listed as changed with the blob id it had at the taken commit", name)
		}
		if !strings.Contains(string(notes), "`web/"+name+"`") {
			t.Errorf("LOCAL_CHANGES.md does not name web/%s", name)
		}
	}
	for _, name := range added {
		if _, ok := changed[name]; ok {
			t.Errorf("%s is listed as changed and as added", name)
		}
		if !skipped[name] {
			t.Errorf("%s is listed as added but not embedded", name)
		}
		if !strings.Contains(string(notes), "`web/"+name+"`") {
			t.Errorf("LOCAL_CHANGES.md does not name web/%s", name)
		}
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
	// 70 files at the taken commit, counted with git ls-tree on 2026-11-12; index.html is served at "/",
	// and every added file is served beside them
	if want := 69 + len(added); served != want {
		t.Fatalf("%d files served below /css/, /js/ and /assets/, want %d", served, want)
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

// File content reaches the page as text nodes only. No script of the interface may hold a call that
// turns a string into markup or code. 59 scripts at the taken commit, counted with git ls-files on
// 2026-11-19, and kinds.js: a walk that reads fewer has missed a folder.
func TestScriptsHoldNoMarkupSink(t *testing.T) {
	for _, problem := range scriptProblems(files) {
		t.Error(problem)
	}
}

// Markup sinks no script of web may hold: each turns a string into
// markup or code.
var sinks = []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "srcdoc",
	"createContextualFragment", "parseFromString", "setHTMLUnsafe"}

var (
	pageScript   = regexp.MustCompile(`<script[^>]*\ssrc="([^"]+)"`)
	importTarget = regexp.MustCompile(`(?:\bfrom\s*|\bimport\s*\(\s*|\bimport\s+|\bmodule:\s*)["'](\.{1,2}/[^"']+\.js)["']`)
)

// scriptProblems reads every script below web of fsys and names each one
// that holds a markup sink. The walk has to read every script the page
// loads: the scripts of index.html, every module a script it read imports
// (also through the module of a route), and every script of the list
// added. So an empty walk, or one that misses a folder, cannot pass.
func scriptProblems(fsys fs.FS) []string {
	var problems []string
	read := map[string]bool{}
	required := map[string]string{} // script -> who names it
	for _, name := range added {
		if path.Ext(name) == ".js" {
			required["web/"+name] = "the list added"
		}
	}
	if page, err := fs.ReadFile(fsys, "web/index.html"); err != nil {
		problems = append(problems, "web/index.html cannot be read: "+err.Error())
	} else {
		for _, m := range pageScript.FindAllSubmatch(page, -1) {
			required[path.Join("web", string(m[1]))] = "web/index.html"
		}
	}
	err := fs.WalkDir(fsys, "web", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(name) != ".js" {
			return err
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		read[name] = true
		for _, sink := range sinks {
			if bytes.Contains(data, []byte(sink)) {
				problems = append(problems, fmt.Sprintf("%s holds %s", name, sink))
			}
		}
		for _, m := range importTarget.FindAllSubmatch(data, -1) {
			required[path.Join(path.Dir(name), string(m[1]))] = name
		}
		return nil
	})
	if err != nil {
		problems = append(problems, err.Error())
	}
	if len(required) == 0 {
		problems = append(problems, "no script is named by web/index.html, an import or the list added")
	}
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !read[name] {
			problems = append(problems, fmt.Sprintf("%s, named by %s, was not read", name, required[name]))
		}
	}
	return problems
}

// The guard itself: each sink in a script of a copy of web is named, an
// empty tree and a tree without js/viewer cannot pass, and the real tree
// passes (the twin).
func TestScriptGuardRefusesEachSinkAndAMissingFolder(t *testing.T) {
	tree := fstest.MapFS{}
	err := fs.WalkDir(files, "web", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(files, name)
		tree[name] = &fstest.MapFile{Data: data}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := scriptProblems(tree); len(got) != 0 {
		t.Fatalf("the copy of web: %q", got)
	}
	viewer := "web/js/viewer/view.js"
	for _, sink := range sinks {
		copied := fstest.MapFS{}
		for name, f := range tree {
			copied[name] = f
		}
		copied[viewer] = &fstest.MapFile{Data: append(append([]byte{}, tree[viewer].Data...), []byte("\n// "+sink+"\n")...)}
		if got := scriptProblems(copied); !strings.Contains(strings.Join(got, "\n"), viewer+" holds "+sink) {
			t.Errorf("a script with %s passes: %q", sink, got)
		}
	}
	empty := fstest.MapFS{"web/index.html": tree["web/index.html"]}
	if len(scriptProblems(empty)) == 0 {
		t.Error("a tree without scripts passes")
	}
	without := fstest.MapFS{}
	for name, f := range tree {
		if !strings.HasPrefix(name, "web/js/viewer/") {
			without[name] = f
		}
	}
	if got := scriptProblems(without); len(got) == 0 {
		t.Error("a tree without js/viewer passes")
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
