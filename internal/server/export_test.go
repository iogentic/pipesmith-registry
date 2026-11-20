package server

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/export"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// exportApp is a server behind the gate, as Serve runs it, over stores of
// an export test.
type exportApp struct {
	t      *testing.T
	cfg    *config.Config
	srv    *Server
	addr   string
	access *syncBuffer
}

type exportOptions struct {
	security model.Security
	limits   export.Limits
	stall    time.Duration
	limit    time.Duration // the time limit of one export
	blobs    []testkit.BlobSpec
	defs     []*pyjson.Object
	// wrap replaces each attached store before the first request
	wrap func(stores.Store) stores.Store
}

func exportDefs() []*pyjson.Object {
	return []*pyjson.Object{
		def("demo-maven", "maven2", "hosted"), def("demo-maven-b", "maven2", "hosted"),
		def("demo-group", "maven2", "group", "demo-maven", "demo-maven-b"),
		def("demo-raw", "raw", "hosted"), def("demo-npm", "npm", "hosted"), dockerDef("demo-docker", 14691),
	}
}

func exportBlobs() []testkit.BlobSpec {
	at := func(s testkit.BlobSpec, created int64) testkit.BlobSpec {
		s.CreatedMS = created
		return s
	}
	lib := "com/example/demo-lib/1.0.0/demo-lib-1.0.0"
	return []testkit.BlobSpec{
		at(spec("demo-maven", lib+".jar", []byte("jar bytes of demo-lib"), "application/java-archive"), createdMS),
		at(spec("demo-maven", lib+".jar.sha1", []byte("0123456789abcdef0123456789abcdef01234567"), "text/plain"), createdMS+1000),
		at(spec("demo-maven", lib+".pom", []byte(strings.Repeat("<project/>\n", 400)), "application/xml"), createdMS+2000),
		at(spec("demo-maven", "com/example/demo-lib/maven-metadata.xml", metadataXML("1.0.0"), "application/xml"), createdMS+3000),
		at(spec("demo-maven", "org/other/thing/1/thing-1.jar", []byte("other"), "application/java-archive"), createdMS),
		at(spec("demo-maven-b", "com/example/demo-lib/maven-metadata.xml", metadataXML("2.0.0"), "application/xml"), createdMS+4000),
		at(spec("demo-maven-b", lib+".jar", []byte("the jar of member b"), "application/java-archive"), createdMS+9000),
		at(spec("demo-maven-b", "com/example/demo-lib/2.0.0/demo-lib-2.0.0.jar", []byte("jar 2"), "application/java-archive"), createdMS),
		spec("demo-raw", "docs/readme.txt", []byte("readme"), "text/plain"),
		spec("demo-raw", "docs/sub/deep.txt", []byte("deep"), "text/plain"),
		spec("demo-raw", "docs-other/no.txt", []byte("no"), "text/plain"),
		spec("demo-raw", "root.txt", []byte("root"), "text/plain"),
		spec("demo-npm", "widget", []byte(`{"name": "widget"}`), "application/json"),
		spec("demo-npm", "widget/-/widget-1.2.3.tgz", []byte("tarball 1.2.3"), "application/x-tgz"),
		spec("demo-npm", "widget/-/widget-1.3.0.tgz", []byte("tarball 1.3.0"), "application/x-tgz"),
		spec("demo-npm", "@demo/util/-/util-2.0.0.tgz", []byte("scoped tarball"), "application/x-tgz"),
		spec("demo-npm", "notes/readme.txt", []byte("no tarball"), "text/plain"),
	}
}

func metadataXML(version string) []byte {
	return []byte(fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata>\n  <groupId>com.example</groupId>\n"+
		"  <artifactId>demo-lib</artifactId>\n  <versioning>\n    <latest>%[1]s</latest>\n    <release>%[1]s</release>\n"+
		"    <versions>\n      <version>%[1]s</version>\n    </versions>\n    <lastUpdated>20231114221320</lastUpdated>\n"+
		"  </versioning>\n</metadata>\n", version))
}

func startExport(t *testing.T, opts exportOptions) *exportApp {
	t.Helper()
	if opts.blobs == nil {
		opts.blobs = exportBlobs()
	}
	if opts.defs == nil {
		opts.defs = exportDefs()
	}
	base := t.TempDir()
	cfg := prepare(t, base, []config.StoreConfig{blobStore(t, base, "store-a", opts.blobs...)}, appOptions{defs: opts.defs})
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = l.Addr().(*net.TCPAddr).Port
	a := &exportApp{t: t, cfg: cfg, addr: l.Addr().String(), access: &syncBuffer{}}
	a.srv, err = New(cfg, nil, Options{AccessLog: a.access, Security: opts.security,
		ExportMaxFiles: opts.limits.MaxFiles, ExportMaxBytes: opts.limits.MaxBytes, ExportStall: opts.stall, ExportTimeLimit: opts.limit})
	if err != nil {
		t.Fatal(err)
	}
	if opts.wrap != nil {
		st := a.srv.rp.current.Load()
		for name, store := range st.stores {
			st.stores[name] = opts.wrap(store)
		}
	}
	hs := newHTTPServer(a.srv)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = hs.Serve(&gateListener{Listener: l, marker: a.srv.marker, escape: a.srv.escape})
	}()
	t.Cleanup(func() {
		_ = hs.Close()
		<-done
		a.srv.Close()
	})
	return a
}

func exportTarget(repo string, values ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(values); i += 2 {
		q.Set(values[i], values[i+1])
	}
	target := "/api/export/" + repo
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	return target
}

func (a *exportApp) get(target string, headers ...string) *answer {
	a.t.Helper()
	return do(a.t, a.addr, "GET", target, nil, headers...)
}

// listing is name, size and bytes of every zip entry, in order.
type zipRow struct {
	name string
	data []byte
}

func readZip(t *testing.T, data []byte) []zipRow {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	var rows []zipRow
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if int64(f.UncompressedSize64) != int64(len(content)) {
			t.Fatalf("%s: size %d, %d bytes", f.Name, f.UncompressedSize64, len(content))
		}
		rows = append(rows, zipRow{f.Name, content})
	}
	return rows
}

func entryNames(rows []zipRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.name)
	}
	return out
}

func walked(t *testing.T, a *exportApp, repo, folder string) []index.Asset {
	t.Helper()
	var out []index.Asset
	if err := a.srv.rp.index.WalkPrefix(repo, folder, func(x index.Asset) error { out = append(out, x); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func dryOf(t *testing.T, a *answer) map[string]any {
	t.Helper()
	status(t, a, 200)
	if ct := a.header.Get("Content-Type"); ct != jsonType {
		t.Fatalf("dry Content-Type %q", ct)
	}
	return a.object(t)
}

func TestExportStreamsThroughTheGate(t *testing.T) {
	a := startExport(t, exportOptions{})
	target := exportTarget("demo-raw", "path", "docs")
	for _, version := range []string{"1.1", "1.0"} {
		conn, err := net.DialTimeout("tcp", a.addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		closing := ""
		if version == "1.1" {
			closing = "Connection: close\r\n"
		}
		fmt.Fprintf(conn, "GET %s HTTP/%s\r\nHost: %s\r\n%s\r\n", target, version, a.addr, closing)
		data, err := io.ReadAll(conn)
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
		head, body, found := bytes.Cut(data, []byte("\r\n\r\n"))
		if !found || !bytes.HasPrefix(head, []byte("HTTP/1.1 200 ")) {
			t.Fatalf("HTTP/%s: %q", version, head)
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(data)), nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get("Content-Length") != "" || resp.ContentLength != -1 {
			t.Fatalf("HTTP/%s: a Content-Length on a stream: %v", version, resp.Header)
		}
		switch version {
		case "1.1":
			if !slices.Equal(resp.TransferEncoding, []string{"chunked"}) {
				t.Fatalf("HTTP/1.1: transfer encoding %v", resp.TransferEncoding)
			}
			body, err = io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
		case "1.0":
			// ReadResponse takes "Connection: close" out of the header into Close
			if !resp.Close || !bytes.Contains(head, []byte("\r\nConnection: close")) || len(resp.TransferEncoding) != 0 {
				t.Fatalf("HTTP/1.0: %v %v", resp.Header, resp.TransferEncoding)
			}
		}
		if got := entryNames(readZip(t, body)); !slices.Equal(got, []string{"readme.txt", "sub/deep.txt"}) {
			t.Fatalf("HTTP/%s: entries %q", version, got)
		}
	}
	lines := strings.Count(a.access.String(), `"GET /api/export/demo-raw?path=docs HTTP/1.`)
	if lines != 2 {
		t.Fatalf("%d access lines for two exports:\n%s", lines, a.access.String())
	}
}

func TestExportForms(t *testing.T) {
	a := startExport(t, exportOptions{})
	cases := []struct {
		repo, folder, form, archive string
		want                        []string
		member                      string
		strip                       string // prefix taken off a stored path for its entry name
	}{
		{"demo-maven", "com/example/demo-lib", "", "demo-maven-demo-lib.zip", nil, "demo-maven", ""},
		{"demo-maven", "com/example/demo-lib", "maven", "demo-maven-demo-lib.zip", nil, "demo-maven", ""},
		{"demo-maven", "", "maven", "demo-maven-root.zip", nil, "demo-maven", ""},
		{"demo-raw", "docs", "", "demo-raw-docs.zip", []string{"readme.txt", "sub/deep.txt"}, "demo-raw", "docs/"},
		{"demo-raw", "", "zip", "demo-raw-root.zip", []string{"root.txt", "docs/readme.txt", "docs-other/no.txt", "docs/sub/deep.txt"}, "demo-raw", ""},
		{"demo-npm", "", "npm", "demo-npm-root.zip", []string{"@demo/util/util-2.0.0.tgz", "widget/widget-1.2.3.tgz", "widget/widget-1.3.0.tgz"}, "", ""},
		{"demo-npm", "widget", "", "demo-npm-widget.zip", []string{"widget/widget-1.2.3.tgz", "widget/widget-1.3.0.tgz"}, "", ""},
	}
	for _, c := range cases {
		values := []string{"path", c.folder}
		if c.form != "" {
			values = append(values, "form", c.form)
		}
		dry := dryOf(t, a.get(exportTarget(c.repo, append(values, "dry", "1")...)))
		got := a.get(exportTarget(c.repo, values...))
		status(t, got, 200)
		h := got.header
		wantType := "application/zip"
		if h.Get("Content-Type") != wantType || h.Get("Content-Disposition") != `attachment; filename="`+c.archive+`"` ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" || h.Get("Content-Length") != "" {
			t.Errorf("%s %s: headers %v", c.repo, c.folder, h)
		}
		rows := readZip(t, got.body)
		var total int64
		for _, r := range rows {
			total += int64(len(r.data))
		}
		if dry["files"] != json.Number(fmt.Sprint(len(rows))) || dry["bytes"] != json.Number(fmt.Sprint(total)) ||
			dry["name"] != c.archive || dry["exact"] != true || dry["repository"] != c.repo || dry["path"] != c.folder {
			t.Errorf("%s %q: dry %v, archive %d files %d bytes", c.repo, c.folder, dry, len(rows), total)
		}
		if c.want != nil && !slices.Equal(entryNames(rows), c.want) {
			t.Errorf("%s %q: entries %q, want %q", c.repo, c.folder, entryNames(rows), c.want)
		}
		if c.member != "" {
			var want []string
			for _, x := range walked(t, a, c.member, c.folder) {
				want = append(want, strings.TrimPrefix(x.Path, c.strip))
			}
			if !slices.Equal(entryNames(rows), want) {
				t.Errorf("%s %q: entries %q, the walk %q", c.repo, c.folder, entryNames(rows), want)
			}
		}
		for _, r := range rows {
			path := c.strip + r.name
			if c.repo == "demo-npm" {
				// <package>/<file>.tgz is stored as <package>/-/<file>.tgz
				cut := strings.LastIndexByte(r.name, '/')
				path = r.name[:cut] + "/-/" + r.name[cut+1:]
			}
			file := a.get("/repository/" + c.repo + "/" + path)
			if file.status != 200 || !bytes.Equal(file.body, r.data) {
				t.Errorf("%s: the entry differs from the GET of %s (%d)", r.name, path, file.status)
			}
		}
	}
	// npm entries hold the bytes of the tarballs
	rows := readZip(t, a.get(exportTarget("demo-npm", "form", "npm")).body)
	if string(rows[0].data) != "scoped tarball" || string(rows[1].data) != "tarball 1.2.3" {
		t.Fatalf("npm entries %q %q", rows[0].data, rows[1].data)
	}
}

func TestExportRefusals(t *testing.T) {
	a := startExport(t, exportOptions{})
	cases := []struct {
		target string
		status int
		text   string
	}{
		{exportTarget("missing"), 404, "repository missing not found"},
		{exportTarget("demo-raw", "path", "nothing/here"), 404, "no folder"},
		{exportTarget("demo-raw", "path", "root.txt"), 404, "no folder"},
		{exportTarget("demo-raw", "path", "../etc"), 400, "segment"},
		{exportTarget("demo-raw", "form", "maven"), 400, "form"},
		{exportTarget("demo-raw", "form", "tar"), 400, "form"},
		{exportTarget("demo-maven", "form", "npm"), 400, "form"},
		{exportTarget("demo-raw", "dry", "yes"), 400, "dry"},
		{exportTarget("demo-npm", "path", "notes"), 404, "nothing"},
	}
	for _, c := range cases {
		got := a.get(c.target)
		if got.status != c.status || !strings.Contains(fmt.Sprint(got.object(t)["message"]), c.text) {
			t.Errorf("%s: %d %s, want %d with %q", c.target, got.status, got.body, c.status, c.text)
		}
		if got.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff on the refusal", c.target)
		}
	}
	// the positive twins of the refusals of form and path
	for _, target := range []string{exportTarget("demo-raw", "form", "zip"), exportTarget("demo-raw", "path", "docs/sub"),
		exportTarget("demo-maven", "form", "maven", "dry", "1"), exportTarget("demo-npm", "path", "widget")} {
		status(t, a.get(target), 200)
	}
}

func TestExportLeavesOutAStoredPathNoArchiveMayHold(t *testing.T) {
	logs := captureLog(t)
	for _, bad := range []string{"docs/../escape.txt", `docs/a\b.txt`, "docs/./x", "docs/c\x01d"} {
		blobs := []testkit.BlobSpec{spec("demo-raw", "docs/fine.txt", []byte("fine"), "text/plain"),
			spec("demo-raw", bad, []byte("bad"), "text/plain")}
		a := startExport(t, exportOptions{blobs: blobs, defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
		if len(walked(t, a, "demo-raw", "docs")) != 2 {
			t.Fatalf("%q did not reach the index", bad)
		}
		dry := dryOf(t, a.get(exportTarget("demo-raw", "path", "docs", "dry", "1")))
		if dry["files"] != json.Number("1") || dry["left_out"] != json.Number("1") || fmt.Sprint(dry["left_out_paths"]) != fmt.Sprint([]any{bad}) {
			t.Fatalf("%q: dry %v", bad, dry)
		}
		got := a.get(exportTarget("demo-raw", "path", "docs"))
		status(t, got, 200)
		if rows := readZip(t, got.body); len(rows) != 1 || rows[0].name != "fine.txt" || string(rows[0].data) != "fine" {
			t.Fatalf("%q: entries %v", bad, entryNames(rows))
		}
		// the folder that holds only the bad path has nothing to export: 409 with that path
		alone := startExport(t, exportOptions{blobs: blobs[1:], defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
		for _, dry := range []string{"", "1"} {
			got := alone.get(exportTarget("demo-raw", "path", "docs", "dry", dry))
			if got.status != 409 || got.object(t)["path"] != bad {
				t.Fatalf("%q alone, dry %q: %d %s", bad, dry, got.status, got.body)
			}
		}
	}
	if !slices.ContainsFunc(logs.all(), func(line string) bool {
		return strings.Contains(line, "1 files, 4 bytes") && strings.Contains(line, "1 left out")
	}) {
		t.Fatalf("no export line counts the left out path:\n%s", strings.Join(logs.all(), "\n"))
	}
	// a folder whose own path holds a backslash: its zip names lie below
	// it and pass, the folder above it holds only the bad path
	inner := []testkit.BlobSpec{spec("demo-raw", `docs/a\b/inner.txt`, []byte("inner"), "text/plain")}
	b := startExport(t, exportOptions{blobs: inner, defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	if rows := readZip(t, b.get(exportTarget("demo-raw", "path", `docs/a\b`)).body); len(rows) != 1 || rows[0].name != "inner.txt" {
		t.Fatalf("the folder with a backslash: %v", entryNames(rows))
	}
	if got := b.get(exportTarget("demo-raw", "path", "docs", "dry", "1")); got.status != 409 || got.object(t)["path"] != `docs/a\b/inner.txt` {
		t.Fatalf("the folder above it: %d %s", got.status, got.body)
	}
}

// The dry call lists at most leftOutShown of the paths it leaves out and
// counts them all.
func TestExportBoundsTheListOfLeftOutPaths(t *testing.T) {
	blobs := []testkit.BlobSpec{spec("demo-raw", "docs/fine.txt", []byte("fine"), "text/plain")}
	for i := 0; i < leftOutShown+5; i++ {
		blobs = append(blobs, spec("demo-raw", fmt.Sprintf(`docs/bad\%03d.txt`, i), []byte("bad"), "text/plain"))
	}
	a := startExport(t, exportOptions{blobs: blobs, defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	dry := dryOf(t, a.get(exportTarget("demo-raw", "path", "docs", "dry", "1")))
	paths, _ := dry["left_out_paths"].([]any)
	if dry["left_out"] != json.Number(fmt.Sprint(leftOutShown+5)) || len(paths) != leftOutShown || paths[0] != `docs/bad\000.txt` {
		t.Fatalf("dry %v", dry)
	}
	// the twin: a folder without such a path lists none
	ok := startExport(t, exportOptions{blobs: blobs[:1], defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	if dry := dryOf(t, ok.get(exportTarget("demo-raw", "path", "docs", "dry", "1"))); dry["left_out"] != json.Number("0") || fmt.Sprint(dry["left_out_paths"]) != "[]" {
		t.Fatalf("clean dry %v", dry)
	}
}

// Two npm packages with a tarball of one file name keep their package path,
// so the archive holds two entries with the bytes of each.
func TestExportNpmTarballsOfOneFileNameStayApart(t *testing.T) {
	blobs := []testkit.BlobSpec{
		spec("demo-npm", "alpha/-/shared-1.0.0.tgz", []byte("tarball of alpha"), "application/x-tgz"),
		spec("demo-npm", "beta/-/shared-1.0.0.tgz", []byte("tarball of beta"), "application/x-tgz"),
	}
	a := startExport(t, exportOptions{blobs: blobs, defs: []*pyjson.Object{def("demo-npm", "npm", "hosted")}})
	got := a.get(exportTarget("demo-npm", "form", "npm"))
	status(t, got, 200)
	rows := readZip(t, got.body)
	var names []string
	for _, r := range rows {
		names = append(names, r.name+"="+string(r.data))
	}
	if want := "[alpha/shared-1.0.0.tgz=tarball of alpha beta/shared-1.0.0.tgz=tarball of beta]"; fmt.Sprint(names) != want {
		t.Fatalf("entries %v, want %s", names, want)
	}
}

// A stored file beside a folder of its name would be the entries a and a/b,
// which no unpack can hold both of: the file stays and the paths below it
// are left out, so the folders above export their other files. The folder
// that holds only such paths answers 409 with the first of them.
func TestExportLeavesOutAPathBelowAFileOfItsName(t *testing.T) {
	blobs := []testkit.BlobSpec{spec("demo-raw", "top/a", []byte("file a"), "text/plain"),
		spec("demo-raw", "top/a/b", []byte("file b"), "text/plain"), spec("demo-raw", "top/c", []byte("file c"), "text/plain")}
	a := startExport(t, exportOptions{blobs: blobs, defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	if len(walked(t, a, "demo-raw", "top")) != 3 {
		t.Fatal("top/a and top/a/b did not both reach the index")
	}
	for _, folder := range []string{"top", ""} {
		dry := dryOf(t, a.get(exportTarget("demo-raw", "path", folder, "dry", "1")))
		if dry["files"] != json.Number("2") || dry["left_out"] != json.Number("1") || fmt.Sprint(dry["left_out_paths"]) != "[top/a/b]" {
			t.Fatalf("%q: dry %v", folder, dry)
		}
		rows := readZip(t, a.get(exportTarget("demo-raw", "path", folder)).body)
		prefix := "top/" // the zip form names entries below the folder
		if folder != "" {
			prefix = ""
		}
		if fmt.Sprint(entryNames(rows)) != fmt.Sprintf("[%sa %sc]", prefix, prefix) || string(rows[0].data) != "file a" {
			t.Fatalf("%q: entries %v", folder, entryNames(rows))
		}
	}
	// the folder below the file exports alone, its names below it hold no clash
	rows := readZip(t, a.get(exportTarget("demo-raw", "path", "top/a")).body)
	if len(rows) != 1 || rows[0].name != "b" {
		t.Fatalf("the folder top/a gave %v", entryNames(rows))
	}
}

func TestExportLimits(t *testing.T) {
	// demo-raw/docs holds 2 files of 10 bytes
	at := startExport(t, exportOptions{limits: export.Limits{MaxFiles: 2, MaxBytes: 10}})
	status(t, at.get(exportTarget("demo-raw", "path", "docs")), 200)
	dry := dryOf(t, at.get(exportTarget("demo-raw", "path", "docs", "dry", "1")))
	if dry["max_files"] != json.Number("2") || dry["max_bytes"] != json.Number("10") {
		t.Fatalf("dry %v", dry)
	}
	for _, limits := range []export.Limits{{MaxFiles: 1, MaxBytes: 10}, {MaxFiles: 2, MaxBytes: 9}} {
		over := startExport(t, exportOptions{limits: limits})
		for _, dry := range []string{"", "1"} {
			got := over.get(exportTarget("demo-raw", "path", "docs", "dry", dry))
			if got.status != 413 {
				t.Fatalf("%+v dry %q: %d %s", limits, dry, got.status, got.body)
			}
			body := got.object(t)
			if body["files"] != json.Number("2") || body["bytes"] != json.Number("10") || body["complete"] != true ||
				body["max_files"] != json.Number(fmt.Sprint(limits.MaxFiles)) || body["max_bytes"] != json.Number(fmt.Sprint(limits.MaxBytes)) {
				t.Fatalf("%+v: %v", limits, body)
			}
			if !strings.Contains(fmt.Sprint(body["message"]), "limit") {
				t.Fatalf("message %v", body["message"])
			}
		}
	}
	defaults := startExport(t, exportOptions{})
	dry = dryOf(t, defaults.get(exportTarget("demo-raw", "dry", "1")))
	if dry["max_files"] != json.Number("20000") || dry["max_bytes"] != json.Number(fmt.Sprint(int64(2<<30))) {
		t.Fatalf("default limits %v", dry)
	}
}

func TestExportPermission(t *testing.T) {
	sec := newFakeSecurity()
	sec.addBasic("raw-browse:pw", subject("raw-browse", "", "nexus:repository-view:raw:demo-raw:browse"))
	sec.addBasic("raw-read:pw", subject("raw-read", "", "nexus:repository-view:raw:demo-raw:browse,read"))
	a := startExport(t, exportOptions{security: sec})
	target := exportTarget("demo-raw", "path", "docs")
	status(t, a.get(target), 401)
	status(t, a.get(target, "basic", "raw-browse:pw"), 403)
	status(t, a.get(target, "basic", asReader), 403) // maven2 only
	got := a.get(target, "basic", "raw-read:pw")
	status(t, got, 200)
	if !slices.Equal(entryNames(readZip(t, got.body)), []string{"readme.txt", "sub/deep.txt"}) {
		t.Fatal("the granted export holds other entries")
	}
	status(t, a.get(target, "basic", asAdmin), 200)
	// a group by its own name, as a download
	status(t, a.get(exportTarget("demo-group", "dry", "1"), "basic", asReader), 200)
	sec.addBasic("member-only:pw", subject("member-only", "", "nexus:repository-view:maven2:demo-maven:browse,read"))
	status(t, a.get(exportTarget("demo-group", "dry", "1"), "basic", "member-only:pw"), 403)
	status(t, a.get("/repository/demo-group/"+jarPath, "basic", "member-only:pw"), 403)
	// anonymous download on: an export still needs a subject (call B17)
	anon := newFakeSecurity()
	anon.anonymous = true
	anon.anonymousPermissions = anonymousRights
	b := startExport(t, exportOptions{security: anon})
	status(t, b.get("/repository/demo-raw/docs/readme.txt"), 200)
	status(t, b.get(target), 401)
	status(t, b.get(target, "basic", asAdmin), 200)
}

// countingStore records every file it opens.
type countingStore struct {
	stores.Store
	mu     sync.Mutex
	opened []*os.File
}

func (c *countingStore) Open(location string) (*os.File, error) {
	f, err := c.Store.Open(location)
	if err == nil {
		c.mu.Lock()
		c.opened = append(c.opened, f)
		c.mu.Unlock()
	}
	return f, err
}

// stillOpen counts the opened files that are not closed.
func (c *countingStore) stillOpen() (opened, open int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.opened {
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			open++
		}
	}
	return len(c.opened), open
}

// largeBlobs are 200 jar files of 128 KiB of random bytes below demo-raw/big:
// stored without compression, 25 MiB in all, more than the socket buffers
// of a connection hold.
func largeBlobs(t *testing.T) []testkit.BlobSpec {
	t.Helper()
	random := mathrand.New(mathrand.NewSource(11))
	var out []testkit.BlobSpec
	for i := 0; i < 200; i++ {
		data := make([]byte, 128*1024)
		random.Read(data)
		out = append(out, spec("demo-raw", fmt.Sprintf("big/f%03d.jar", i), data, "application/java-archive"))
	}
	return append(out, spec("demo-raw", "small/one.txt", []byte("one"), "text/plain"))
}

// openStream sends an export request and reads the head and the first n
// bytes of the body; the connection stays open for the caller.
func openStream(t *testing.T, addr, target string, n int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, addr)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", err, resp)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func waitWithin(t *testing.T, what string, within time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExportClientLeavesInTheMiddle(t *testing.T) {
	var counted *countingStore
	a := startExport(t, exportOptions{blobs: largeBlobs(t), defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")},
		wrap: func(s stores.Store) stores.Store { counted = &countingStore{Store: s}; return counted }})
	target := exportTarget("demo-raw", "path", "big")
	conn := openStream(t, a.addr, target, 64*1024)
	if opened, _ := counted.stillOpen(); opened == 0 || opened == 200 {
		t.Fatalf("%d files opened after the first 64 KiB", opened)
	}
	conn.Close()
	waitWithin(t, "the end of the handler", 5*time.Second, func() bool { return len(a.srv.exports.slots) == 0 })
	opened, open := counted.stillOpen()
	if open != 0 || opened >= 200 {
		t.Fatalf("%d of %d opened files are still open", open, opened)
	}
	got := a.get(target)
	status(t, got, 200)
	if rows := readZip(t, got.body); len(rows) != 200 {
		t.Fatalf("the next export holds %d entries", len(rows))
	}
	if _, open := counted.stillOpen(); open != 0 {
		t.Fatalf("%d files open after a whole export", open)
	}
}

func TestExportTwoStreamsAtATime(t *testing.T) {
	a := startExport(t, exportOptions{blobs: largeBlobs(t), defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	target := exportTarget("demo-raw", "path", "big")
	slow := []net.Conn{openStream(t, a.addr, target, 1024), openStream(t, a.addr, target, 1024)}
	waitWithin(t, "two streams", 5*time.Second, func() bool { return len(a.srv.exports.slots) == 2 })
	third := a.get(target)
	if third.status != 429 || third.header.Get("Retry-After") != "30" {
		t.Fatalf("third export: %d %v %q", third.status, third.header, third.body)
	}
	if !strings.Contains(fmt.Sprint(third.object(t)["message"]), "two exports") {
		t.Fatalf("message %s", third.body)
	}
	// a dry call does not take a place, and a small export waits for one as well
	dryOf(t, a.get(exportTarget("demo-raw", "path", "big", "dry", "1")))
	status(t, a.get(exportTarget("demo-raw", "path", "small")), 429)
	for _, c := range slow {
		c.Close()
	}
	waitWithin(t, "free places", 5*time.Second, func() bool { return len(a.srv.exports.slots) == 0 })
	status(t, a.get(exportTarget("demo-raw", "path", "small")), 200)
}

func TestExportGroup(t *testing.T) {
	a := startExport(t, exportOptions{})
	folder := "com/example/demo-lib"
	dry := dryOf(t, a.get(exportTarget("demo-group", "path", folder, "dry", "1")))
	if dry["exact"] != false || dry["form"] != "maven" || dry["name"] != "demo-group-demo-lib.zip" {
		t.Fatalf("dry %v", dry)
	}
	got := a.get(exportTarget("demo-group", "path", folder))
	status(t, got, 200)
	rows := readZip(t, got.body)
	seen := map[string]int{}
	for _, r := range rows {
		seen[r.name]++
		file := a.get("/repository/demo-group/" + r.name)
		if file.status != 200 || !bytes.Equal(file.body, r.data) {
			t.Errorf("%s: the entry differs from the GET of the group (%d)", r.name, file.status)
		}
	}
	lib := folder + "/1.0.0/demo-lib-1.0.0.jar"
	metadata := folder + "/maven-metadata.xml"
	want := []string{metadata, folder + "/1.0.0/demo-lib-1.0.0.jar", folder + "/1.0.0/demo-lib-1.0.0.jar.sha1",
		folder + "/1.0.0/demo-lib-1.0.0.pom", folder + "/2.0.0/demo-lib-2.0.0.jar"}
	if !slices.Equal(entryNames(rows), want) {
		t.Fatalf("entries %q, want %q", entryNames(rows), want)
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("%s stands %d times", name, n)
		}
	}
	for _, r := range rows {
		switch r.name {
		case lib:
			// both members hold it, member b with the newer row: the first member wins, as in a download
			if string(r.data) != "jar bytes of demo-lib" {
				t.Errorf("%s holds %q, the bytes of the second member", lib, r.data)
			}
		case metadata:
			// the merged document of the group, which neither member stores
			if !bytes.Contains(r.data, []byte("<version>1.0.0</version>")) || !bytes.Contains(r.data, []byte("<version>2.0.0</version>")) {
				t.Errorf("the metadata of the group is not merged: %s", r.data)
			}
		}
	}
	if int64(len(rows)) != mustInt(t, dry["files"]) {
		t.Errorf("dry files %v, archive %d", dry["files"], len(rows))
	}
	// the listing takes the row of the first member too: the entry carries its time
	r, err := zip.NewReader(bytes.NewReader(got.body), int64(len(got.body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.File {
		if f.Name == lib && !f.Modified.Equal(time.UnixMilli(createdMS)) {
			t.Errorf("%s has the time %v, not the one of the first member", lib, f.Modified.UTC())
		}
	}
}

func mustInt(t *testing.T, v any) int64 {
	t.Helper()
	n, err := v.(json.Number).Int64()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The log line of an export quotes the values of the request and of the
// store: a stored folder whose name holds a line break gives one line, not
// a line of its own in the server log. The twin: a plain folder reads as
// before, with quotes.
func TestExportLogLineQuotesTheFolder(t *testing.T) {
	logs := captureLog(t)
	folder := "evil\n2026-11-19 12:00:00,000 INFO pipesmith_registry.server: export of admin ok"
	blobs := []testkit.BlobSpec{spec("demo-raw", folder+"/f.txt", []byte("f"), "text/plain"),
		spec("demo-raw", "docs/readme.txt", []byte("readme"), "text/plain")}
	a := startExport(t, exportOptions{blobs: blobs, defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	status(t, a.get(exportTarget("demo-raw", "path", folder)), 200)
	status(t, a.get(exportTarget("demo-raw", "path", "docs")), 200)
	var lines []string
	for _, record := range logs.all() {
		if strings.Contains(record, "export of") {
			lines = append(lines, strings.Split(record, "\n")...)
		}
	}
	want := []string{
		`INFO pipesmith_registry.server: export of "demo-raw" path "evil\n2026-11-19 12:00:00,000 INFO pipesmith_registry.server: export of admin ok" form "zip": 1 files, 1 bytes, `,
		`INFO pipesmith_registry.server: export of "demo-raw" path "docs" form "zip": 1 files, 6 bytes, `,
	}
	if len(lines) != len(want) {
		t.Fatalf("%d log lines of the two exports, want %d: %q", len(lines), len(want), lines)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, want[i]) || !strings.HasSuffix(line, " ms") {
			t.Errorf("log line %q, want it to start with %q", line, want[i])
		}
	}
}

// An npm tarball whose stored package path holds a dot segment, which a
// GET refuses, answers 409 with its stored path: the entry keeps the
// package path, so the entry name guard sees the segment. The twin: the
// tarball of a plain package path exports.
func TestExportNpmLeavesOutADotSegmentInThePackagePath(t *testing.T) {
	for _, bad := range []string{"../-/escape-1.0.0.tgz", "./-/dot-1.0.0.tgz", "@demo/../-/scoped-1.0.0.tgz"} {
		blobs := []testkit.BlobSpec{spec("demo-npm", "fine/-/fine-1.0.0.tgz", []byte("fine"), "application/x-tgz"),
			spec("demo-npm", bad, []byte("bad"), "application/x-tgz")}
		a := startExport(t, exportOptions{blobs: blobs, defs: []*pyjson.Object{def("demo-npm", "npm", "hosted")}})
		dry := dryOf(t, a.get(exportTarget("demo-npm", "form", "npm", "dry", "1")))
		if dry["left_out"] != json.Number("1") || fmt.Sprint(dry["left_out_paths"]) != fmt.Sprint([]any{bad}) {
			t.Fatalf("%q: dry %v", bad, dry)
		}
		if rows := readZip(t, a.get(exportTarget("demo-npm", "form", "npm")).body); len(rows) != 1 || rows[0].name != "fine/fine-1.0.0.tgz" {
			t.Fatalf("%q: entries %v", bad, entryNames(rows))
		}
		alone := startExport(t, exportOptions{blobs: blobs[1:], defs: []*pyjson.Object{def("demo-npm", "npm", "hosted")}})
		if got := alone.get(exportTarget("demo-npm", "form", "npm")); got.status != 409 || got.object(t)["path"] != bad {
			t.Fatalf("%q alone: %d %s", bad, got.status, got.body)
		}
		ok := startExport(t, exportOptions{blobs: blobs[:1], defs: []*pyjson.Object{def("demo-npm", "npm", "hosted")}})
		rows := readZip(t, ok.get(exportTarget("demo-npm", "form", "npm")).body)
		if len(rows) != 1 || rows[0].name != "fine/fine-1.0.0.tgz" {
			t.Fatalf("the plain package gave %v", rows)
		}
	}
}

// A maven2 group counts a merged maven-metadata.xml with the size of the
// merge, which is larger than the document of either member: the dry call
// gives the bytes the archive holds, and a byte limit of one byte less
// refuses the export. The twin: the limit at the merged size exports.
func TestExportGroupCountsTheMergedMetadata(t *testing.T) {
	folder := "com/example/demo-lib"
	merged := bodyOf(t, startExport(t, exportOptions{}).get("/repository/demo-group/"+folder+"/maven-metadata.xml"))
	if len(merged) <= len(metadataXML("1.0.0")) {
		t.Fatalf("the merge has %d bytes, the member %d", len(merged), len(metadataXML("1.0.0")))
	}
	a := startExport(t, exportOptions{})
	dry := dryOf(t, a.get(exportTarget("demo-group", "path", folder, "dry", "1")))
	rows := readZip(t, a.get(exportTarget("demo-group", "path", folder)).body)
	var total int64
	for _, r := range rows {
		total += int64(len(r.data))
		if r.name == folder+"/maven-metadata.xml" && !bytes.Equal(r.data, merged) {
			t.Fatalf("the entry is not the merge: %s", r.data)
		}
	}
	if dry["bytes"] != json.Number(fmt.Sprint(total)) || dry["files"] != json.Number(fmt.Sprint(len(rows))) {
		t.Fatalf("dry %v, archive %d files %d bytes", dry, len(rows), total)
	}
	over := startExport(t, exportOptions{limits: export.Limits{MaxFiles: 100, MaxBytes: total - 1}})
	if got := over.get(exportTarget("demo-group", "path", folder)); got.status != 413 {
		t.Fatalf("one byte below the archive: %d %s", got.status, got.body)
	}
	at := startExport(t, exportOptions{limits: export.Limits{MaxFiles: 100, MaxBytes: total}})
	if rows := readZip(t, at.get(exportTarget("demo-group", "path", folder)).body); len(rows) != 5 {
		t.Fatalf("at the limit: %d entries", len(rows))
	}
}

// bodyOf is the body of a 200 answer.
func bodyOf(t *testing.T, got *answer) []byte {
	t.Helper()
	status(t, got, 200)
	return got.body
}
