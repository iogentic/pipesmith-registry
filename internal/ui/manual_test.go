package ui

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
)

func mustNewManual(t *testing.T) *Handler {
	t.Helper()
	h, err := NewManual()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// inManualSet applies manualSet, the rule tools/sync_manual.py reads, to a
// path below docs/.
func inManualSet(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
			return false
		}
	}
	for _, rule := range manualSet {
		if folder, ok := strings.CutSuffix(rule, "**"); ok {
			if strings.HasPrefix(name, folder) {
				return true
			}
			continue
		}
		dir, glob := path.Split(rule)
		if matched, _ := path.Match(glob, path.Base(name)); matched && path.Dir(name) == path.Clean("./"+dir) {
			return true
		}
	}
	return false
}

func readTree(t *testing.T, fsys fs.FS, root string, keep func(string) bool) map[string][]byte {
	t.Helper()
	tree := map[string][]byte{}
	err := fs.WalkDir(fsys, root, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(name, root), "/")
		if !keep(rel) {
			return nil
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		tree[rel] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// The embedded manual is a byte copy of the manual set of docs/.
func TestManualIsTheCopyOfTheManualSetOfDocs(t *testing.T) {
	docs := readTree(t, os.DirFS("../../docs"), ".", inManualSet)
	embedded := readTree(t, manualFiles, "manual", func(string) bool { return true })
	if len(docs) == 0 {
		t.Fatal("the manual set of docs/ is empty")
	}
	for name, data := range docs {
		copy, ok := embedded[name]
		switch {
		case !ok:
			t.Errorf("docs/%s is missing in internal/ui/manual: run python3 tools/sync_manual.py", name)
		case !bytes.Equal(copy, data):
			t.Errorf("internal/ui/manual/%s differs from docs/%s: run python3 tools/sync_manual.py", name, name)
		}
	}
	for name := range embedded {
		if _, ok := docs[name]; !ok {
			t.Errorf("internal/ui/manual/%s is not in the manual set of docs/: run python3 tools/sync_manual.py", name)
		}
	}
}

func TestEveryManualFileIsServedAtItsPath(t *testing.T) {
	h := mustNewManual(t)
	embedded := readTree(t, manualFiles, "manual", func(string) bool { return true })
	for name, data := range embedded {
		target := "/manual/" + name
		if name == "index.html" {
			target = "/manual/"
		}
		w := serve(t, h, "GET", target)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
			t.Errorf("GET %s: %d, %d bytes, want 200 and %d bytes", target, w.Code, w.Body.Len(), len(data))
		}
		checkHeaders(t, w, manualTypes[path.Ext(name)])
		head := serve(t, h, "HEAD", target)
		if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != fmt.Sprint(len(data)) {
			t.Errorf("HEAD %s: %d, body %d bytes, Content-Length %q", target, head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
		}
	}
	if _, ok := embedded["index.html"]; !ok {
		t.Fatal("the embedded manual holds no index.html")
	}
}

func TestManualWithoutSlashRedirects(t *testing.T) {
	h := mustNewManual(t)
	if !h.Has("/manual") {
		t.Fatal(`Has("/manual") is false`)
	}
	for _, method := range []string{"GET", "HEAD"} {
		w := serve(t, h, method, "/manual")
		if w.Code != 308 || w.Header().Get("Location") != "manual/" || w.Body.Len() != 0 && method == "HEAD" {
			t.Errorf("%s /manual: %d, Location %q", method, w.Code, w.Header().Get("Location"))
		}
	}
	if w := serve(t, h, "POST", "/manual"); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /manual: %d, Allow %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestOnlyTheManualSetIsServed(t *testing.T) {
	h := mustNewManual(t)
	for _, target := range []string{"/manual/index.html", "/manual/missing.css", "/manual/../index.html", "/manual/assets/",
		"/manual/assets", "/", "/js/app.js", "/manual//", "/MANUAL/", "/manual/manual.css/"} {
		if h.Has(target) {
			t.Errorf("Has(%q) is true", target)
		}
		if w := serve(t, h, "GET", target); w.Code != 404 {
			t.Errorf("GET %s: %d, want 404", target, w.Code)
		}
	}
	for _, target := range []string{"/manual/", "/manual/manual.css", "/manual/manual.js", "/manual/assets/architecture.svg"} {
		if !h.Has(target) {
			t.Errorf("Has(%q) is false", target)
		}
	}
	ui := mustNew(t)
	for _, target := range []string{"/manual", "/manual/", "/manual/manual.css"} {
		if ui.Has(target) {
			t.Errorf("the interface handler has %q", target)
		}
	}
}

func TestOtherMethodsAreRefusedInTheManual(t *testing.T) {
	h := mustNewManual(t)
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		w := serve(t, h, method, "/manual/manual.css")
		if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d, Allow %q", method, w.Code, w.Header().Get("Allow"))
		}
	}
}

// The manual is served under the policy of the interface, so its pages may
// hold no inline script or style, and they load nothing from another host.
func TestManualPagesHoldNoInlineCodeAndNoOtherHost(t *testing.T) {
	inline := regexp.MustCompile(`(?i)<script(\s[^>]*)?>\s*[^<\s]|<style|\sstyle=|\son[a-z]+=`)
	remote := regexp.MustCompile(`(?i)\ssrc=["']?(https?:|//)|<link\s[^>]*href=["']?(https?:|//)`)
	pages := 0
	for name, data := range readTree(t, manualFiles, "manual", func(name string) bool { return path.Ext(name) == ".html" }) {
		pages++
		if m := inline.Find(data); m != nil {
			t.Errorf("manual/%s holds %q", name, m)
		}
		if m := remote.Find(data); m != nil {
			t.Errorf("manual/%s loads from another host: %q", name, m)
		}
	}
	if pages == 0 {
		t.Fatal("no manual page found")
	}
}

// The manual names the version of VERSION in its header and its lead, and the
// API page names no version in its markup (api.js reads it from openapi.json).
func TestManualNamesTheVersionOfVERSION(t *testing.T) {
	raw, err := os.ReadFile("../../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(raw))
	index, err := fs.ReadFile(manualFiles, "manual/index.html")
	if err != nil {
		t.Fatal(err)
	}
	header := regexp.MustCompile(`(?s)<header class="top">.*?</header>`).Find(index)
	if !bytes.Contains(header, []byte("Manual for "+version+"<")) {
		t.Errorf("the header of index.html does not hold %q: %s", "Manual for "+version, header)
	}
	lead := regexp.MustCompile(`<p class="lead">([^<]*)`).FindSubmatch(index)
	if lead == nil || !strings.HasPrefix(string(lead[1]), "Version "+version+". ") {
		t.Errorf("the lead of index.html does not start with %q: %q", "Version "+version+".", lead)
	}
	api, err := fs.ReadFile(manualFiles, "manual/api.html")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`\b\d+\.\d+\.\d+\b`).Find(api); m != nil {
		t.Errorf("api.html names the version %q in its markup", m)
	}
}

// The scripts of the manual build no markup from text: the names below are
// sinks that turn a string into elements.
var markupSinks = []string{"DOMParser", "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "parseFromString"}

func TestManualScriptsUseNoMarkupSink(t *testing.T) {
	embedded := readTree(t, manualFiles, "manual", func(name string) bool { return path.Ext(name) == ".js" })
	if len(embedded) == 0 {
		t.Fatal("the embedded manual holds no script")
	}
	for name, data := range embedded {
		for _, sink := range markupSinks {
			if bytes.Contains(data, []byte(sink)) {
				t.Errorf("internal/ui/manual/%s names %s", name, sink)
			}
		}
	}
}

const manualPDFPath = "/manual/pdf/pipesmith-registry-manual.pdf"

// The PDF of the manual is a download: its own type and an attachment header,
// so a browser saves it and never shows it under the policy of the page.
func TestTheManualPDFIsServedAsAnAttachment(t *testing.T) {
	h := mustNewManual(t)
	data, err := fs.ReadFile(manualFiles, "manual/pdf/pipesmith-registry-manual.pdf")
	if err != nil {
		t.Fatalf("the embedded manual holds no PDF: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		t.Fatalf("the embedded PDF starts with %q", data[:min(len(data), 8)])
	}
	const disposition = `attachment; filename="pipesmith-registry-manual.pdf"`
	w := serve(t, h, "GET", manualPDFPath)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("GET %s: %d, %d bytes, want 200 and %d bytes", manualPDFPath, w.Code, w.Body.Len(), len(data))
	}
	checkHeaders(t, w, "application/pdf")
	if got := w.Header().Get("Content-Disposition"); got != disposition {
		t.Errorf("GET %s: Content-Disposition %q, want %q", manualPDFPath, got, disposition)
	}
	head := serve(t, h, "HEAD", manualPDFPath)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != fmt.Sprint(len(data)) ||
		head.Header().Get("Content-Disposition") != disposition {
		t.Errorf("HEAD %s: %d, body %d bytes, Content-Length %q, Content-Disposition %q", manualPDFPath, head.Code,
			head.Body.Len(), head.Header().Get("Content-Length"), head.Header().Get("Content-Disposition"))
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		other := serve(t, h, method, manualPDFPath)
		if other.Code != 405 || other.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s %s: %d, Allow %q, want 405 and GET, HEAD", method, manualPDFPath, other.Code, other.Header().Get("Allow"))
		}
	}
	page := serve(t, h, "GET", "/manual/")
	if page.Header().Get("Content-Disposition") != "" {
		t.Errorf("GET /manual/ carries Content-Disposition %q", page.Header().Get("Content-Disposition"))
	}
}

// The row of serve in the table of the chapter Commands names every option
// that internal/cli/cli.go gives serve, and no other. The options are read
// from the source as text, because this package does not import internal/cli.
func TestManualNamesEveryOptionOfServe(t *testing.T) {
	source, err := os.ReadFile("../cli/cli.go")
	if err != nil {
		t.Fatal(err)
	}
	code := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*serve\.add\(&action\{[^\n]*options: \[\]string\{([^}]*)\}`).FindAllSubmatch(source, -1) {
		for _, option := range regexp.MustCompile(`"(--?[a-z-]+)"`).FindAllSubmatch(m[1], -1) {
			code[string(option[1])] = true
		}
	}
	if len(code) < 4 {
		t.Fatalf("internal/cli/cli.go gives serve %d options, want at least 4: %v", len(code), code)
	}
	index, err := fs.ReadFile(manualFiles, "manual/index.html")
	if err != nil {
		t.Fatal(err)
	}
	commands := regexp.MustCompile(`(?s)<section id="commands">.*?</section>`).Find(index)
	row := regexp.MustCompile(`<tr><td><code>serve</code></td><td>(.*?)</td>`).FindSubmatch(commands)
	if row == nil {
		t.Fatal("the table of the chapter Commands has no row of serve")
	}
	manual := map[string]bool{}
	for _, m := range regexp.MustCompile(`<code>(--?[a-z-]+)[ <]`).FindAllSubmatch(row[1], -1) {
		manual[string(m[1])] = true
	}
	for option := range code {
		if !manual[option] {
			t.Errorf("the row of serve does not name %s, an option of serve in internal/cli/cli.go", option)
		}
	}
	for option := range manual {
		if !code[option] {
			t.Errorf("the row of serve names %s, which internal/cli/cli.go does not give serve", option)
		}
	}
}
