package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/md5"  // #nosec G501 -- the .md5 file of a Maven upload, not a security use.
	"crypto/sha1" // #nosec G505 -- the .sha1 file of a Maven upload, not a security use.
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The REST writes of task 15 of the phase 4 plan. The fields and answers
// are those of the public documentation of the old product, pages
// Components API (sections Upload Component, Delete Component), Assets API
// (section Delete Asset) and Uploading Components, read 2026-11-18: an
// upload and a delete answer 204, a docker repository takes no upload. Every
// refusal status is provisional (W6).

const (
	componentsPath = "/service/rest/v1/components"
	writablePath   = "/service/rest/v1/status/writable"
)

// restRepos are the repositories of startREST: hosted ones of the four
// formats, a raw one under DENY and a maven2 group.
var restRepos = []struct {
	name, format, kind string
	members            []string
}{
	{"demo-raw", "raw", "hosted", nil},
	{"demo-raw-readonly", "raw", "hosted", nil},
	{"demo-releases", "maven2", "hosted", nil},
	{"demo-npm", "npm", "hosted", nil},
	{"demo-docker", "docker", "hosted", nil},
	{"demo-group", "maven2", "group", []string{"demo-releases"}},
	{"demo-npm-group", "npm", "group", []string{"demo-npm"}},
}

// restAttributes are the stored attributes of the repositories of
// startREST unless a test gives others.
func restAttributes() map[string]map[string]any {
	return map[string]map[string]any{
		"demo-raw":          storage("ALLOW"),
		"demo-raw-readonly": storage("DENY"),
		"demo-releases": {"storage": map[string]any{"writePolicy": "ALLOW_ONCE"},
			"maven": map[string]any{"versionPolicy": "RELEASE", "layoutPolicy": "STRICT"}},
		"demo-npm":    storage("ALLOW"),
		"demo-docker": storage("ALLOW"),
	}
}

// startREST is a server with security and writes on over the store store-a
// of the old product, with the repositories of restRepos. demo-adder holds
// add, read and browse of every repository, no edit and no delete.
func startREST(t *testing.T, attributes map[string]map[string]any, blobs ...testkit.BlobSpec) *writing {
	t.Helper()
	sec := newFakeSecurity()
	sec.addBasic(asAdder, subject("demo-adder", "", "nexus:repository-view:*:*:add,read,browse", "nexus:search:read"))
	snap := *sec.snap
	snap.Repositories = nil
	var defs []*pyjson.Object
	for _, r := range restRepos {
		snap.Repositories = append(snap.Repositories, model.Repository{Name: r.name, Format: r.format, Type: r.kind, Online: true,
			Attributes: attributes[r.name]})
		defs = append(defs, def(r.name, r.format, r.kind, r.members...))
	}
	sec.snap = &snap
	base := t.TempDir()
	storeCfgs := []config.StoreConfig{blobStore(t, base, "store-a", blobs...)}
	w := &writing{catalog: &fakeCatalog{}, clock: &clock{now: time.Date(2026, 11, 18, 10, 15, 30, 123_000_000, time.UTC)},
		old: storeCfgs[0].Root}
	opts := appOptions{defs: defs, security: sec, writes: true, catalog: w.catalog, writeOptions: blobwrite.Options{Now: w.clock.Now}}
	w.app = startApp(t, base, prepare(t, base, storeCfgs, opts), opts)
	return w
}

// formPart is one part of a multipart body; a file has a file name.
type formPart struct {
	name, file string
	data       []byte
}

func formField(name, value string) formPart { return formPart{name: name, data: []byte(value)} }

func formFile(name, file string, data []byte) formPart {
	return formPart{name: name, file: file, data: data}
}

// multipartBody is a body of the parts and its Content-Type.
func multipartBody(t *testing.T, parts ...formPart) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		var w io.Writer
		var err error
		if p.file != "" {
			w, err = mw.CreateFormFile(p.name, p.file)
		} else {
			w, err = mw.CreateFormField(p.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(p.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), mw.FormDataContentType()
}

// upload sends an upload of the parts to a repository as a user.
func (w *writing) upload(repo, who string, parts ...formPart) *answer {
	w.t.Helper()
	body, contentType := multipartBody(w.t, parts...)
	return do(w.t, w.addr(), "POST", componentsPath+"?repository="+url.QueryEscape(repo), body, "basic", who, "Content-Type", contentType)
}

func (w *writing) fetch(repo, path string) *answer {
	w.t.Helper()
	return do(w.t, w.addr(), "GET", "/repository/"+repo+"/"+path, nil, "basic", asAdmin)
}

// noUploadLeft checks that a refused upload left no work file and no blob
// of the write store.
func (w *writing) noUploadLeft(what string) {
	w.t.Helper()
	if files := w.workFiles(); len(files) != 0 {
		w.t.Errorf("%s: work files %v", what, files)
	}
	if counts, _ := w.scan(); counts.Live != 0 {
		w.t.Errorf("%s: the write store holds %+v", what, counts)
	}
}

// fieldErrors is the list of a 400 of an upload: field id to message.
func fieldErrors(t *testing.T, r any) map[string]string {
	t.Helper()
	var body []byte
	switch a := r.(type) {
	case *answer:
		body = a.body
	case *response:
		body = a.body
	}
	var list []struct{ ID, Message string }
	if err := json.Unmarshal(body, &list); err != nil || len(list) == 0 {
		t.Fatalf("no list of field errors: %q", body)
	}
	out := map[string]string{}
	for _, e := range list {
		out[e.ID] = e.Message
	}
	return out
}

// postWhileSending sends a POST of an upload as admin on a connection of
// its own and reads the answer while the body is still being sent: the
// server may answer and close before the body ends.
func postWhileSending(t *testing.T, addr, target, contentType string, body []byte) *response {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte(asAdmin))
	go func() {
		fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Basic %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
			target, auth, contentType, len(body))
		_, _ = conn.Write(body) // the server may close first
	}()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.Close {
		resp.Header.Set("Connection", "close") // ReadResponse takes the header out and sets Close
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: data}
}

// With writes off GET /v1/status/writable answers the 503 of 0.1.1 with
// and without security; with writes on 200, and 503 once the directories a
// write creates files in may not be written.
func TestWritableStatus(t *testing.T) {
	for _, a := range []*app{startWriting(t, nil, false), startWriting(t, newFakeSecurity(), false)} {
		r := a.get(writablePath)
		if r.status != 503 || len(r.body) != 0 || r.header.Get("Content-Type") != "" || r.header.Get("Content-Length") != "0" {
			t.Fatalf("writes off: %s", r)
		}
	}
	on := startWriting(t, newFakeSecurity(), true)
	if r := on.get(writablePath); r.status != 200 || len(r.body) != 0 || r.header.Get("Content-Length") != "0" {
		t.Fatalf("writes on: %s", r)
	}
	root := writeStoreRoot(on.cfg.DataDir)
	dirs := []string{filepath.Join(root, "content", "tmp"), filepath.Join(root, "content"), filepath.Join(root, "reconciliation")}
	if got := on.srv.writes.writeDirs(); !reflect.DeepEqual(got, dirs) {
		t.Fatalf("the directories of a write: %v", got)
	}
	// each directory that may not be written makes the store unwritable;
	// an access check that refuses stands in for the modes, which root
	// writes in spite of
	for _, refused := range dirs {
		if on.srv.writes.writableBy(func(path string, mode uint32) error {
			if path == refused && mode == accessWrite {
				return syscall.EACCES
			}
			return nil
		}) {
			t.Errorf("writable with %s refused", refused)
		}
	}
	if !on.srv.writes.writableBy(func(string, uint32) error { return nil }) {
		t.Fatal("not writable with every directory writable")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory without the write bit; the modes are left to the black box test")
	}
	for _, dir := range dirs {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, dir := range dirs {
			_ = os.Chmod(dir, 0o750) // so that the temporary directory can go
		}
	})
	if r := on.get(writablePath); r.status != 503 || len(r.body) != 0 {
		t.Fatalf("store read only: %s", r)
	}
	for _, dir := range dirs {
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if r := on.get(writablePath); r.status != 200 {
		t.Fatalf("store writable again: %s", r)
	}
}

// The two upload field lists: every format that takes an upload, and one
// format; their fields are the multipart fields the upload takes.
func TestUploadSpecs(t *testing.T) {
	on := startWriting(t, newFakeSecurity(), true)
	r := do(t, on.addr(), "GET", "/service/rest/v1/formats/upload-specs", nil, "basic", asReader)
	status(t, r, 200)
	var specs []struct {
		Format          string `json:"format"`
		MultipleUpload  bool   `json:"multipleUpload"`
		ComponentFields []struct {
			Name, Type, Description string
			Optional                bool
			Group                   *string
		} `json:"componentFields"`
		AssetFields []struct {
			Name, Type, Description string
			Optional                bool
			Group                   *string
		} `json:"assetFields"`
	}
	dec := json.NewDecoder(bytes.NewReader(r.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&specs); err != nil {
		t.Fatalf("%v: %s", err, r.body)
	}
	var formats []string
	for _, spec := range specs {
		formats = append(formats, spec.Format)
		var names []string
		for _, f := range spec.ComponentFields {
			names = append(names, spec.Format+"."+f.Name)
		}
		assets := []string{".asset"}
		if spec.MultipleUpload {
			assets = []string{".asset1", ".asset2", ".asset3"}
		}
		for _, asset := range assets {
			names = append(names, spec.Format+asset)
			for _, f := range spec.AssetFields {
				names = append(names, spec.Format+asset+"."+f.Name)
			}
		}
		if want := rest.UploadFieldNames(spec.Format); !reflect.DeepEqual(names, want) {
			t.Errorf("%s: the field list gives %v, the upload takes %v", spec.Format, names, want)
		}
		one := do(t, on.addr(), "GET", "/service/rest/v1/formats/"+spec.Format+"/upload-specs", nil, "basic", asReader)
		status(t, one, 200)
		if whole, _ := json.Marshal(rest.UploadSpec(spec.Format).Body); string(one.body) != string(whole) {
			t.Errorf("%s: %s, want %s", spec.Format, one.body, whole)
		}
	}
	if !reflect.DeepEqual(formats, []string{"maven2", "npm", "raw"}) {
		t.Fatalf("formats %v", formats)
	}
	for _, format := range []string{"docker", "pypi"} {
		status(t, do(t, on.addr(), "GET", "/service/rest/v1/formats/"+format+"/upload-specs", nil, "basic", asAdmin), 404)
	}
	status(t, do(t, on.addr(), "GET", "/service/rest/v1/formats/upload-specs", nil), 401)
	// writes off: both field lists are refusals (TestTheListOfUploadSpecsIsARefusalWithWritesOff)
	off := startWriting(t, newFakeSecurity(), false)
	for _, target := range []string{"/service/rest/v1/formats/maven2/upload-specs", "/service/rest/v1/formats/upload-specs"} {
		if refused := do(t, off.addr(), "GET", target, nil, "basic", asAdmin); refused.status != 501 || refused.object(t)["area"] != "content" {
			t.Fatalf("writes off: %s: %d %q", target, refused.status, refused.body)
		}
	}
}

// With writes off and security on, GET /formats/upload-specs answers as the
// field list of one format and the other REST writes do (decision of the
// planning session, 2026-11-19): 401 with the same challenge without
// credentials, the 501 of the area content with them, and an entry of the
// list of GET /api/areas and of the route list. With writes on it is the
// field list of every format (TestUploadSpecs), and with security off it
// stays 404 (TestRESTWritesWithWritesOff).
func TestTheListOfUploadSpecsIsARefusalWithWritesOff(t *testing.T) {
	off := startWriting(t, newFakeSecurity(), false)
	list, one := "/service/rest/v1/formats/upload-specs", "/service/rest/v1/formats/raw/upload-specs"
	for _, c := range []struct {
		name    string
		headers []string
		status  int
	}{
		{"no credentials", nil, 401},
		{"a browser without a session", []string{"Sec-Fetch-Mode", "cors"}, 401},
		{"a reader", []string{"basic", asReader}, 501},
		{"an administrator", []string{"basic", asAdmin}, 501},
	} {
		got, want := do(t, off.addr(), "GET", list, nil, c.headers...), do(t, off.addr(), "GET", one, nil, c.headers...)
		for _, a := range []*answer{got, want} {
			a.header.Del("Date")
			a.header.Del("Content-Length") // the 501 names the path of its request
		}
		switch body := strings.ReplaceAll(string(got.body), list, one); {
		case got.status != c.status || want.status != c.status:
			t.Errorf("%s: %d and %d, want %d: %q", c.name, got.status, want.status, c.status, got.body)
		case !reflect.DeepEqual(got.header, want.header):
			t.Errorf("%s: headers %v, the field list of one format %v", c.name, got.header, want.header)
		case body != string(want.body):
			t.Errorf("%s: %q, the field list of one format %q", c.name, got.body, want.body)
		}
		if c.status == 501 {
			if m := got.object(t); m["area"] != "content" || m["message"] != "GET "+list+" is not available in this server yet" || len(m) != 2 {
				t.Errorf("%s: %#v", c.name, m)
			}
		}
	}
	var areas struct {
		Refused []rest.Refused `json:"refused"`
	}
	if r := do(t, off.addr(), "GET", "/api/areas", nil, "basic", asAdmin); r.status != 200 || json.Unmarshal(r.body, &areas) != nil ||
		!slices.Contains(areas.Refused, rest.Refused{Method: "GET", Path: list, Area: "content"}) {
		t.Errorf("GET /api/areas: %d %s", r.status, r.body)
	}
	for _, writes := range []bool{false, true} {
		var tables []string
		for _, info := range Routes(writes) {
			if slices.Contains(info.Examples, list) {
				tables = append(tables, info.Table+" "+info.Rule)
			}
		}
		want := map[bool][]string{false: {"refusals signed-in"}, true: {"writes signed-in"}}[writes]
		if !slices.Equal(tables, want) {
			t.Errorf("writes %v: the route list names %s as %v, want %v", writes, list, tables, want)
		}
	}
}

// A raw upload: the files below the directory, as blobs of the write store
// with the uploader, served by the read path.
func TestRESTUploadRaw(t *testing.T) {
	w := startREST(t, restAttributes())
	r := w.upload("demo-raw", asAdmin, formField("raw.directory", "/docs/up/"), formFile("raw.asset1", "local-a.txt", []byte("alpha")),
		formField("raw.asset1.filename", "a.txt"), formFile("raw.asset2", "local-b.txt", []byte("beta")), formField("raw.asset2.filename", "b.txt"))
	if r.status != 204 || len(r.body) != 0 {
		t.Fatalf("%d %q", r.status, r.body)
	}
	for path, want := range map[string]string{"docs/up/a.txt": "alpha", "docs/up/b.txt": "beta"} {
		if got := w.fetch("demo-raw", path); got.status != 200 || string(got.body) != want {
			t.Fatalf("%s: %d %q", path, got.status, got.body)
		}
		row, _ := w.srv.rp.index.Lookup("demo-raw", path)
		if row == nil || row.Store != "local-write" {
			t.Fatalf("%s: %+v", path, row)
		}
		if props := w.properties(row.Location); props["@BlobStore.created-by"] != "demo-admin" || props["@BlobStore.blob-name"] != "/"+path {
			t.Fatalf("%s: %q", path, props)
		}
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
	// the root of the repository as the directory
	if r := w.upload("demo-raw", asAdmin, formField("raw.directory", "/"), formFile("raw.asset1", "x", []byte("top")),
		formField("raw.asset1.filename", "top.txt")); r.status != 204 {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if got := w.fetch("demo-raw", "top.txt"); string(got.body) != "top" {
		t.Fatalf("%d %q", got.status, got.body)
	}
}

const pomOfUpload = `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <parent><groupId>com.example</groupId><artifactId>demo-parent</artifactId><version>2.0.0</version></parent>
  <artifactId>demo-pom</artifactId>
  <packaging>jar</packaging>
</project>
`

func hexMD5(data []byte) string {
	sum := md5.Sum(data) // #nosec G401 -- see the import.
	return hex.EncodeToString(sum[:])
}

func hexSHA1(data []byte) string {
	sum := sha1.Sum(data) // #nosec G401 -- see the import.
	return hex.EncodeToString(sum[:])
}

// A maven2 upload with a POM made from the coordinates: the files at the
// paths of the Maven layout, each with its .md5 and .sha1, and the
// metadata of the artifact lists the version. A second upload of the same
// component under ALLOW_ONCE is refused before a byte is written.
func TestRESTUploadMavenGeneratesThePOM(t *testing.T) {
	w := startREST(t, restAttributes())
	jar, sources := []byte("PK\x03\x04 the jar"), []byte("PK\x03\x04 the sources")
	parts := []formPart{formField("maven2.groupId", "com.example"), formField("maven2.artifactId", "demo-up"), formField("maven2.version", "1.0.0"),
		formField("maven2.generate-pom", "true"), formFile("maven2.asset1", "demo.jar", jar), formField("maven2.asset1.extension", "jar"),
		formFile("maven2.asset2", "demo-sources.jar", sources), formField("maven2.asset2.extension", "jar"),
		formField("maven2.asset2.classifier", "sources")}
	r := w.upload("demo-releases", asAdmin, parts...)
	if r.status != 204 {
		t.Fatalf("%d %q", r.status, r.body)
	}
	dir := "com/example/demo-up/1.0.0/"
	pom := w.fetch("demo-releases", dir+"demo-up-1.0.0.pom")
	for _, want := range []string{"<groupId>com.example</groupId>", "<artifactId>demo-up</artifactId>", "<version>1.0.0</version>",
		"<packaging>jar</packaging>", "<modelVersion>4.0.0</modelVersion>"} {
		if pom.status != 200 || !strings.Contains(string(pom.body), want) {
			t.Fatalf("the POM lacks %s: %d %q", want, pom.status, pom.body)
		}
	}
	for path, data := range map[string][]byte{dir + "demo-up-1.0.0.jar": jar, dir + "demo-up-1.0.0-sources.jar": sources, dir + "demo-up-1.0.0.pom": pom.body} {
		if got := w.fetch("demo-releases", path); got.status != 200 || !bytes.Equal(got.body, data) {
			t.Fatalf("%s: %d %q", path, got.status, got.body)
		}
		for suffix, sum := range map[string]string{".md5": hexMD5(data), ".sha1": hexSHA1(data)} {
			row, _ := w.srv.rp.index.Lookup("demo-releases", path+suffix)
			if got := w.fetch("demo-releases", path+suffix); row == nil || row.Store != "local-write" || string(got.body) != sum {
				t.Fatalf("%s%s: %+v %q, want %s", path, suffix, row, got.body, sum)
			}
		}
	}
	metadata := w.fetch("demo-releases", "com/example/demo-up/maven-metadata.xml")
	if metadata.status != 200 || !strings.Contains(string(metadata.body), "<version>1.0.0</version>") {
		t.Fatalf("metadata %d %q", metadata.status, metadata.body)
	}
	before, _ := w.scan()
	again := w.upload("demo-releases", asAdmin, parts...)
	if again.status != 400 || fieldErrors(t, again)["maven2.asset1"] == "" {
		t.Fatalf("second upload: %d %q", again.status, again.body)
	}
	if after, _ := w.scan(); after != before {
		t.Fatalf("the second upload wrote %+v, before %+v", after, before)
	}
}

// A POM among the files gives the coordinates, its parent the group and the
// version; a field that says otherwise is refused.
func TestRESTUploadMavenReadsThePOM(t *testing.T) {
	w := startREST(t, restAttributes())
	jar := []byte("PK\x03\x04 jar of the POM")
	r := w.upload("demo-releases", asAdmin, formFile("maven2.asset1", "pom.xml", []byte(pomOfUpload)), formField("maven2.asset1.extension", "pom"),
		formFile("maven2.asset2", "x.jar", jar), formField("maven2.asset2.extension", "jar"))
	if r.status != 204 {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if got := w.fetch("demo-releases", "com/example/demo-pom/2.0.0/demo-pom-2.0.0.jar"); !bytes.Equal(got.body, jar) {
		t.Fatalf("%d %q", got.status, got.body)
	}
	if got := w.fetch("demo-releases", "com/example/demo-pom/2.0.0/demo-pom-2.0.0.pom"); string(got.body) != pomOfUpload {
		t.Fatalf("%d %q", got.status, got.body)
	}
	differs := w.upload("demo-releases", asAdmin, formField("maven2.version", "3.0.0"), formFile("maven2.asset1", "pom.xml", []byte(pomOfUpload)),
		formField("maven2.asset1.extension", "pom"))
	if differs.status != 400 || fieldErrors(t, differs)["maven2.version"] == "" {
		t.Fatalf("%d %q", differs.status, differs.body)
	}
}

// packageTarball is a gzip tar with package/package.json of a name and a version.
func packageTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	manifest := []byte(fmt.Sprintf(`{"name": %q, "version": %q, "description": "an upload of the tests"}`, name, version))
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, f := range []struct {
		name string
		data []byte
	}{{"package/package.json", manifest}, {"package/index.js", []byte("module.exports = 1;\n")}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data)), ModTime: time.Unix(1_700_000_000, 0),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// An npm upload is a publish of the tarball under the name and version of
// its package.json, scoped or not.
func TestRESTUploadNpm(t *testing.T) {
	w := startREST(t, restAttributes())
	for _, c := range []struct{ name, docPath string }{{"demo-up", "demo-up"}, {"@demo/up-scoped", "@demo%2fup-scoped"}} {
		tarball := packageTarball(t, c.name, "1.0.0")
		r := w.upload("demo-npm", asAdmin, formFile("npm.asset", "package.tgz", tarball))
		if r.status != 204 {
			t.Fatalf("%s: %d %q", c.name, r.status, r.body)
		}
		doc := w.fetch("demo-npm", c.docPath)
		var document struct {
			Name     string                    `json:"name"`
			DistTags map[string]string         `json:"dist-tags"`
			Versions map[string]map[string]any `json:"versions"`
		}
		if err := json.Unmarshal(doc.body, &document); err != nil || doc.status != 200 {
			t.Fatalf("%s: %d %q", c.name, doc.status, doc.body)
		}
		v := document.Versions["1.0.0"]
		dist, _ := v["dist"].(map[string]any)
		if document.Name != c.name || document.DistTags["latest"] != "1.0.0" || v["description"] != "an upload of the tests" || dist == nil ||
			dist["shasum"] != hexSHA1(tarball) {
			t.Fatalf("%s: %s", c.name, doc.body)
		}
		unscoped := c.name[strings.LastIndex(c.name, "/")+1:]
		if got := w.fetch("demo-npm", c.name+"/-/"+unscoped+"-1.0.0.tgz"); !bytes.Equal(got.body, tarball) {
			t.Fatalf("%s: tarball %d", c.name, got.status)
		}
	}
}

// Refusals of an upload: before the body is read, of its fields, and of the
// rights. None leaves a work file or a blob.
func TestRESTUploadRefusals(t *testing.T) {
	w := startREST(t, restAttributes())
	one := func(name string, data []byte) []formPart {
		return []formPart{formField("raw.directory", "docs"), formFile("raw.asset1", "f", data), formField("raw.asset1.filename", name)}
	}
	post := func(target string, body []byte, headers ...string) *answer {
		return do(t, w.addr(), "POST", target, body, append([]string{"basic", asAdmin}, headers...)...)
	}
	body, contentType := multipartBody(t, one("a.txt", []byte("a"))...)
	for _, c := range []struct {
		what, target, contentType string
		want                      int
	}{
		{"no repository", componentsPath, contentType, 422},
		{"two repositories", componentsPath + "?repository=demo-raw&repository=demo-npm", contentType, 422},
		{"an unknown repository", componentsPath + "?repository=missing", contentType, 404},
		{"a group", componentsPath + "?repository=demo-group", contentType, 400},
		{"write policy DENY", componentsPath + "?repository=demo-raw-readonly", contentType, 400},
		{"no multipart form", componentsPath + "?repository=demo-raw", "text/plain", 415},
		{"no boundary", componentsPath + "?repository=demo-raw", "multipart/form-data", 415},
	} {
		if r := post(c.target, body, "Content-Type", c.contentType); r.status != c.want {
			t.Errorf("%s: %d %q, want %d", c.what, r.status, r.body, c.want)
		}
	}
	docker := w.upload("demo-docker", asAdmin, formFile("docker.asset", "image.tar", []byte("layers")))
	if docker.status != 400 || !strings.Contains(docker.object(t)["message"].(string), "registry protocol") {
		t.Errorf("docker: %d %q", docker.status, docker.body)
	}
	long := strings.Repeat("x", maxUploadFieldBytes+1)
	for _, c := range []struct {
		what  string
		repo  string
		parts []formPart
		field string
	}{
		{"a field of no format", "demo-raw", append(one("a.txt", []byte("a")), formField("raw.colour", "blue")), "raw.colour"},
		{"a field of another format", "demo-raw", append(one("a.txt", []byte("a")), formField("maven2.groupId", "com.example")), "maven2.groupId"},
		{"a field twice", "demo-raw", append(one("a.txt", []byte("a")), formField("raw.directory", "other")), "raw.directory"},
		{"a fourth asset", "demo-raw", append(one("a.txt", []byte("a")), formFile("raw.asset4", "f", []byte("d"))), "raw.asset4"},
		{"no directory", "demo-raw", one("a.txt", []byte("a"))[1:], "raw.directory"},
		{"a file name with a path", "demo-raw", one("../etc/x.txt", []byte("a")), "raw.asset1.filename"},
		{"a file name with a folder", "demo-raw", one("sub/x.txt", []byte("a")), "raw.asset1.filename"},
		{"a file name with a backslash", "demo-raw", one(`..\x.txt`, []byte("a")), "raw.asset1.filename"},
		{"a directory up", "demo-raw", []formPart{formField("raw.directory", "docs/../.."), formFile("raw.asset1", "f", []byte("a")),
			formField("raw.asset1.filename", "a.txt")}, "raw.directory"},
		{"a file without its name", "demo-raw", one("a.txt", []byte("a"))[:2], "raw.asset1.filename"},
		{"no file", "demo-raw", []formPart{formField("raw.directory", "docs")}, "raw.asset1"},
		{"a field above the bound", "demo-raw", []formPart{formField("raw.directory", long)}, "raw.directory"},
		{"a Maven file without its extension", "demo-releases", []formPart{formField("maven2.groupId", "com.example"),
			formField("maven2.artifactId", "a"), formField("maven2.version", "1.0"), formFile("maven2.asset1", "a.jar", []byte("PK"))},
			"maven2.asset1.extension"},
		{"Maven coordinates missing", "demo-releases", []formPart{formFile("maven2.asset1", "a.jar", []byte("PK")),
			formField("maven2.asset1.extension", "jar")}, "maven2.groupId"},
		{"a group with a slash", "demo-releases", []formPart{formField("maven2.groupId", "com/example"), formField("maven2.artifactId", "a"),
			formField("maven2.version", "1.0"), formFile("maven2.asset1", "a.jar", []byte("PK")), formField("maven2.asset1.extension", "jar")},
			"maven2.groupId"},
		{"generate-pom neither true nor false", "demo-releases", []formPart{formField("maven2.groupId", "com.example"),
			formField("maven2.artifactId", "a"), formField("maven2.version", "1.0"), formField("maven2.generate-pom", "maybe"),
			formFile("maven2.asset1", "a.jar", []byte("PK")), formField("maven2.asset1.extension", "jar")}, "maven2.generate-pom"},
		{"a tarball without package.json", "demo-npm", []formPart{formFile("npm.asset", "x.tgz", []byte("no gzip"))}, "npm.asset"},
	} {
		r := w.upload(c.repo, asAdmin, c.parts...)
		if r.status != 400 {
			t.Errorf("%s: %d %q", c.what, r.status, r.body)
			continue
		}
		if errs := fieldErrors(t, r); errs[c.field] == "" {
			t.Errorf("%s: %v names no %s", c.what, errs, c.field)
		}
	}
	// ten thousand fields, all of them names of the field list: refused at
	// the second, which names a field again, before the rest is read
	many := []formPart{formFile("raw.asset1", "f", []byte("a")), formField("raw.asset1.filename", "a.txt")}
	for range 10_000 {
		many = append(many, formField("raw.directory", "docs"))
	}
	for _, c := range []struct {
		what  string
		parts []formPart
	}{{"ten thousand fields", many}, {"ten thousand fields of no format", append(append(many[:2:2], formField("raw.f0", "v")), many[2:]...)}} {
		body, contentType := multipartBody(t, c.parts...)
		r := postWhileSending(t, w.addr(), componentsPath+"?repository=demo-raw", contentType, body)
		if r.status != 400 || r.header.Get("Connection") != "close" || len(fieldErrors(t, r)) != 1 {
			t.Errorf("%s: %s", c.what, r)
		}
	}
	w.noUploadLeft("the refusals")
	// the rights
	status(t, w.upload("demo-raw", asReader, one("a.txt", []byte("a"))...), 403)
	r := do(t, w.addr(), "POST", componentsPath+"?repository=demo-raw", body, "Content-Type", contentType)
	if r.status != 401 || !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("anonymous: %d %v", r.status, r.header)
	}
	if r := do(t, w.addr(), "POST", componentsPath+"?repository=demo-raw", body, "Cookie", "fake-session=s1", "Content-Type", contentType); r.status != 403 ||
		r.object(t)["message"] != csrfRefusal {
		t.Fatalf("a session without the request token: %d %q", r.status, r.body)
	}
	w.noUploadLeft("the rights")
	status(t, w.upload("demo-raw", asAdder, one("a.txt", []byte("first"))...), 204)
	if r := w.upload("demo-raw", asAdder, one("a.txt", []byte("second"))...); r.status != 403 {
		t.Fatalf("an overwrite without edit: %d %q", r.status, r.body)
	}
	if got := w.fetch("demo-raw", "docs/a.txt"); string(got.body) != "first" {
		t.Fatalf("%q", got.body)
	}
}

// A file above the limit of the repository is refused with 413 while it
// streams; nothing of it stays.
func TestRESTUploadAboveTheLimit(t *testing.T) {
	attrs := restAttributes()
	attrs["demo-raw"] = map[string]any{"storage": map[string]any{"writePolicy": "ALLOW"}, "max_body_bytes": 64}
	w := startREST(t, attrs)
	body, contentType := multipartBody(t, formField("raw.directory", "docs"), formFile("raw.asset1", "big", bytes.Repeat([]byte("b"), 1000)),
		formField("raw.asset1.filename", "big.bin"))
	r := postWhileSending(t, w.addr(), componentsPath+"?repository=demo-raw", contentType, body)
	if r.status != 413 || r.header.Get("Connection") != "close" {
		t.Fatalf("%d %v %q", r.status, r.header, r.body)
	}
	w.noUploadLeft("413")
}

// A part that stops arriving is given up after the chunk timeout with 408,
// and one cut off with the connection leaves nothing either.
func TestRESTUploadOfAStalledBody(t *testing.T) {
	chunkTimeout.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { chunkTimeout.Store(0) })
	w := startREST(t, restAttributes())
	head := "--b\r\nContent-Disposition: form-data; name=\"raw.directory\"\r\n\r\ndocs\r\n" +
		"--b\r\nContent-Disposition: form-data; name=\"raw.asset1\"; filename=\"f\"\r\n\r\nthe first bytes of a part that never ends"
	send := func() net.Conn {
		conn, err := net.Dial("tcp", w.addr())
		if err != nil {
			t.Fatal(err)
		}
		auth := base64.StdEncoding.EncodeToString([]byte(asAdmin))
		fmt.Fprintf(conn, "POST %s?repository=demo-raw HTTP/1.1\r\nHost: localhost\r\nAuthorization: Basic %s\r\n"+
			"Content-Type: multipart/form-data; boundary=b\r\nContent-Length: %d\r\n\r\n%s", componentsPath, auth, 1<<20, head)
		return conn
	}
	conn := send()
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestTimeout || !strings.Contains(string(data), "did not arrive in full") {
		t.Fatalf("%d %q", resp.StatusCode, data)
	}
	waitFor(t, "no work file", func() bool { return len(w.workFiles()) == 0 })
	cut := send()
	cut.Close()
	waitFor(t, "two uploads in the access log", func() bool { return strings.Count(w.access.String(), "POST "+componentsPath) == 2 })
	waitFor(t, "no work file after the cut", func() bool { return len(w.workFiles()) == 0 })
	w.noUploadLeft("a stalled and a cut body")
}

// searchComponent is the id of the one component of a version, or of any
// version when it is empty, that a search of a repository by name finds.
func (w *writing) searchComponent(repo, name, version string) string {
	w.t.Helper()
	q := url.Values{"repository": {repo}, "name": {name}}
	if version != "" {
		q.Set("version", version)
	}
	r := do(w.t, w.addr(), "GET", "/service/rest/v1/search?"+q.Encode(), nil, "basic", asAdmin)
	items, _ := r.object(w.t)["items"].([]any)
	if r.status != 200 || len(items) != 1 {
		w.t.Fatalf("search %s %s %s: %d %q", repo, name, version, r.status, r.body)
	}
	return items[0].(map[string]any)["id"].(string)
}

func (w *writing) deleteREST(target, who string) *answer {
	w.t.Helper()
	return do(w.t, w.addr(), "DELETE", target, nil, "basic", who)
}

// An asset delete by id: the delete mark on the blob, the row out of the
// index, the catalog hook, 204; the refusals of the id, the repository, the
// old store, the write policy and the rights.
func TestRESTDeleteAsset(t *testing.T) {
	w := startREST(t, restAttributes(), spec("demo-raw", "docs/old.txt", []byte("old"), "text/plain"))
	status(t, w.upload("demo-raw", asAdmin, formField("raw.directory", "docs"), formFile("raw.asset1", "f", []byte("alpha")),
		formField("raw.asset1.filename", "a.txt")), 204)
	row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt")
	if row == nil {
		t.Fatal("no row of the upload")
	}
	target := "/service/rest/v1/assets/" + rest.AssetID("demo-raw", "docs/a.txt")
	status(t, w.deleteREST(target, asAdder), 403)
	status(t, w.deleteREST(target, asReader), 403)
	status(t, do(t, w.addr(), "DELETE", target, nil), 401)
	r := w.deleteREST(target, asAdmin)
	if r.status != 204 || len(r.body) != 0 {
		t.Fatalf("%d %q", r.status, r.body)
	}
	status(t, w.fetch("demo-raw", "docs/a.txt"), 404)
	if props := w.properties(row.Location); props["deleted"] != "true" {
		t.Fatalf("%q", props)
	}
	if !strings.Contains(strings.Join(w.catalog.deletes, " "), "demo-raw/docs/a.txt") {
		t.Fatalf("catalog %q", w.catalog.deletes)
	}
	if r := w.deleteREST(target, asAdmin); r.status != 404 || r.object(t)["message"] != "asset not found" {
		t.Fatalf("again: %d %q", r.status, r.body)
	}
	before, _ := testkit.TreeDigest(w.old)
	for _, c := range []struct {
		what, id string
		want     int
	}{
		{"a malformed id", "not-an-id!", 422},
		{"an id of no repository", rest.AssetID("missing", "docs/a.txt"), 404},
		{"an id of a group", rest.AssetID("demo-group", jarPath), 400},
		{"a path of the old store", rest.AssetID("demo-raw", "docs/old.txt"), 409},
		{"write policy DENY", rest.AssetID("demo-raw-readonly", "docs/a.txt"), 400},
	} {
		if r := w.deleteREST("/service/rest/v1/assets/"+c.id, asAdmin); r.status != c.want {
			t.Errorf("%s: %d %q, want %d", c.what, r.status, r.body, c.want)
		}
	}
	if after, _ := testkit.TreeDigest(w.old); after != before {
		t.Fatal("the store of the old product changed")
	}
	if got := w.fetch("demo-raw", "docs/old.txt"); string(got.body) != "old" {
		t.Fatalf("%d %q", got.status, got.body)
	}
}

// A component delete by the id the search gives: every asset of the
// component of raw, maven2, npm and docker, and nothing else.
func TestRESTDeleteComponent(t *testing.T) {
	w := startREST(t, restAttributes(), spec("demo-raw", "docs/old.txt", []byte("old"), "text/plain"))
	// raw: the component is one file
	status(t, w.upload("demo-raw", asAdmin, formField("raw.directory", "docs"), formFile("raw.asset1", "f", []byte("c")),
		formField("raw.asset1.filename", "c.txt"), formFile("raw.asset2", "f", []byte("d")), formField("raw.asset2.filename", "d.txt")), 204)
	id := w.searchComponent("demo-raw", "docs/c.txt", "")
	status(t, w.deleteREST(componentsPath+"/"+id, asAdder), 403)
	if r := w.deleteREST(componentsPath+"/"+id, asAdmin); r.status != 204 || len(r.body) != 0 {
		t.Fatalf("raw: %d %q", r.status, r.body)
	}
	status(t, w.fetch("demo-raw", "docs/c.txt"), 404)
	status(t, w.fetch("demo-raw", "docs/d.txt"), 200)
	status(t, w.deleteREST(componentsPath+"/"+id, asAdmin), 404)
	// maven2: the files of the version with their checksum files
	status(t, w.upload("demo-releases", asAdmin, formField("maven2.groupId", "com.example"), formField("maven2.artifactId", "demo-del"),
		formField("maven2.version", "1.0.0"), formField("maven2.generate-pom", "true"), formFile("maven2.asset1", "a.jar", []byte("PK\x03\x04 del")),
		formField("maven2.asset1.extension", "jar")), 204)
	dir := "com/example/demo-del/1.0.0/demo-del-1.0.0"
	var locations []string
	for _, suffix := range []string{".jar", ".jar.md5", ".jar.sha1", ".pom", ".pom.md5", ".pom.sha1"} {
		row, _ := w.srv.rp.index.Lookup("demo-releases", dir+suffix)
		if row == nil {
			t.Fatalf("no row of %s%s", dir, suffix)
		}
		locations = append(locations, row.Location)
	}
	status(t, w.deleteREST(componentsPath+"/"+w.searchComponent("demo-releases", "demo-del", "1.0.0"), asAdmin), 204)
	for i, suffix := range []string{".jar", ".jar.md5", ".jar.sha1", ".pom", ".pom.md5", ".pom.sha1"} {
		status(t, w.fetch("demo-releases", dir+suffix), 404)
		if props := w.properties(locations[i]); props["deleted"] != "true" {
			t.Fatalf("%s%s: %q", dir, suffix, props)
		}
	}
	// npm: one version goes from the document, the last one takes the document with it
	for _, version := range []string{"1.0.0", "1.1.0"} {
		status(t, w.upload("demo-npm", asAdmin, formFile("npm.asset", "p.tgz", packageTarball(t, "demo-del", version))), 204)
	}
	status(t, w.deleteREST(componentsPath+"/"+w.searchComponent("demo-npm", "demo-del", "1.1.0"), asAdmin), 204)
	doc := w.fetch("demo-npm", "demo-del")
	var document struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]any    `json:"versions"`
	}
	if err := json.Unmarshal(doc.body, &document); err != nil || len(document.Versions) != 1 || document.Versions["1.0.0"] == nil ||
		document.DistTags["latest"] != "1.0.0" {
		t.Fatalf("npm: %d %q", doc.status, doc.body)
	}
	status(t, w.fetch("demo-npm", "demo-del/-/demo-del-1.1.0.tgz"), 404)
	status(t, w.deleteREST(componentsPath+"/"+w.searchComponent("demo-npm", "demo-del", "1.0.0"), asAdmin), 204)
	status(t, w.fetch("demo-npm", "demo-del"), 404)
	if row, _ := w.srv.rp.index.Lookup("demo-npm", "demo-del"); row != nil {
		t.Fatalf("the document stays: %+v", row)
	}
	// docker: by tag the tag only, by digest the digest and the tags of its bytes
	manifest := []byte(`{"schemaVersion": 2, "mediaType": "` + dockerManifestType + `", "config": {}, "layers": []}`)
	digest := digestOf(manifest)
	for _, path := range []string{"v2/app/manifests/1.0", "v2/app/manifests/2.0", "v2/app/manifests/" + digest} {
		if _, err := w.ingest("demo-docker", path, string(manifest)); err != nil {
			t.Fatal(err)
		}
	}
	status(t, w.deleteREST(componentsPath+"/"+w.searchComponent("demo-docker", "app", "1.0"), asAdmin), 204)
	if row, _ := w.srv.rp.index.Lookup("demo-docker", "v2/app/manifests/2.0"); row == nil {
		t.Fatal("the delete of a tag took another tag")
	}
	status(t, w.deleteREST(componentsPath+"/"+w.searchComponent("demo-docker", "app", digest), asAdmin), 204)
	for _, path := range []string{"v2/app/manifests/1.0", "v2/app/manifests/2.0", "v2/app/manifests/" + digest} {
		if row, _ := w.srv.rp.index.Lookup("demo-docker", path); row != nil {
			t.Fatalf("%s stays: %+v", path, row)
		}
	}
	// refusals
	groupID, _ := rest.ComponentID("demo-group", "maven2", jarPath)
	nothing, _ := rest.ComponentID("demo-raw", "raw", "docs/never.txt")
	old, _ := rest.ComponentID("demo-raw", "raw", "docs/old.txt")
	missing, _ := rest.ComponentID("missing", "raw", "docs/a.txt")
	before, _ := testkit.TreeDigest(w.old)
	for _, c := range []struct {
		what, id string
		want     int
	}{
		{"a malformed id", "not-an-id!", 422},
		{"an asset id", rest.AssetID("demo-raw", "docs/d.txt"), 422},
		{"an id of no repository", missing, 404},
		{"an id of a group", groupID, 400},
		{"an id that names nothing", nothing, 404},
		{"a component of the old store", old, 409},
	} {
		if r := w.deleteREST(componentsPath+"/"+c.id, asAdmin); r.status != c.want {
			t.Errorf("%s: %d %q, want %d", c.what, r.status, r.body, c.want)
		}
	}
	if after, _ := testkit.TreeDigest(w.old); after != before {
		t.Fatal("the store of the old product changed")
	}
	status(t, do(t, w.addr(), "DELETE", componentsPath+"/"+old, nil), 401)
}

// With writes off the REST writes answer as 0.1.1: the 501 of their
// refusal with security on, the 404 of 0.1.1 for the component delete,
// which the contract of the interface does not name, although the read of a
// component serves its path since F2 (TestTheComponentDeleteKeepsThe404OfWritesOff),
// and every request 404 with security off. The field list of every format answers like a refusal with
// security on since 2026-11-19.
func TestRESTWritesWithWritesOff(t *testing.T) {
	notFound := `{"detail":"Not Found"}`
	component, _ := rest.ComponentID("demo-raw", "raw", "docs/readme.txt")
	secured := startWriting(t, newFakeSecurity(), false)
	plain := startWriting(t, nil, false)
	for _, c := range []struct {
		method, target string
		status         int
		area           string
	}{
		{"POST", componentsPath + "?repository=demo-raw", 501, "content"},
		{"DELETE", "/service/rest/v1/assets/" + rest.AssetID("demo-raw", "docs/readme.txt"), 501, "content"},
		{"GET", "/service/rest/v1/formats/raw/upload-specs", 501, "content"},
		{"DELETE", componentsPath + "/" + component, 404, ""},
		{"GET", "/service/rest/v1/formats/upload-specs", 501, "content"},
	} {
		r := do(t, secured.addr(), c.method, c.target, []byte("x"), "basic", asAdmin)
		switch {
		case r.status != c.status:
			t.Errorf("%s %s: %d %q, want %d", c.method, c.target, r.status, r.body, c.status)
		case c.area != "" && r.object(t)["area"] != c.area:
			t.Errorf("%s %s: %q", c.method, c.target, r.body)
		case c.area == "" && string(r.body) != notFound:
			t.Errorf("%s %s: %q", c.method, c.target, r.body)
		}
		if r := do(t, plain.addr(), c.method, c.target, []byte("x")); r.status != 404 || string(r.body) != notFound {
			t.Errorf("security off: %s %s: %d %q", c.method, c.target, r.status, r.body)
		}
	}
	if r := do(t, plain.addr(), "GET", "/repository/demo-raw/docs/readme.txt", nil); r.status != 200 {
		t.Fatalf("%d", r.status)
	}
}

// Task 2 of leg 4d (W15, task 10 of the phase 4 plan): after the REST delete
// of a maven2 component the metadata of its artifact is written again and
// no longer lists the version; once no version stays, it goes.
func TestRESTDeleteOfAMavenComponentRebuildsTheMetadata(t *testing.T) {
	w := startREST(t, restAttributes())
	for _, version := range []string{"1.0.0", "1.1.0"} {
		status(t, w.upload("demo-releases", asAdmin, formField("maven2.groupId", "com.example"), formField("maven2.artifactId", "demo-meta"),
			formField("maven2.version", version), formField("maven2.generate-pom", "true"),
			formFile("maven2.asset1", "a.jar", []byte("PK\x03\x04 "+version)), formField("maven2.asset1.extension", "jar")), 204)
	}
	const metadataPath = "com/example/demo-meta/maven-metadata.xml"
	listed := func() string {
		r := w.fetch("demo-releases", metadataPath)
		if r.status != 200 {
			t.Fatalf("metadata: %d %q", r.status, r.body)
		}
		return string(r.body)
	}
	if m := listed(); !strings.Contains(m, "<version>1.0.0</version>") || !strings.Contains(m, "<version>1.1.0</version>") {
		t.Fatalf("before the delete: %s", m)
	}
	status(t, w.deleteREST(componentsPath+"/"+w.searchComponent("demo-releases", "demo-meta", "1.0.0"), asAdmin), 204)
	m := listed()
	if strings.Contains(m, "<version>1.0.0</version>") || !strings.Contains(m, "<version>1.1.0</version>") ||
		!strings.Contains(m, "<release>1.1.0</release>") {
		t.Fatalf("after the delete of 1.0.0: %s", m)
	}
	for _, suffix := range []string{".md5", ".sha1"} {
		if r := w.fetch("demo-releases", metadataPath+suffix); r.status != 200 || len(r.body) == 0 {
			t.Fatalf("%s: %d %q", suffix, r.status, r.body)
		}
	}
	status(t, w.deleteREST(componentsPath+"/"+w.searchComponent("demo-releases", "demo-meta", "1.1.0"), asAdmin), 204)
	status(t, w.fetch("demo-releases", metadataPath), 404)
}

// With writes off and security on, DELETE /v1/components/{id} keeps the 404
// of 0.1.1, with or without credentials, alike to a path no route takes,
// although the read of the component now takes the path; every other method
// the path does not take still answers 405 with Allow GET.
func TestTheComponentDeleteKeepsThe404OfWritesOff(t *testing.T) {
	off := startWriting(t, newFakeSecurity(), false)
	target := "/service/rest/v1/components/ZGVtby1yYXc6AAAAAAAAAAAAAAAAAAAAAA"
	unknown := do(t, off.addr(), "DELETE", "/service/rest/v1/no-such-route", nil, "basic", asAdmin)
	unknown.header.Del("Date")
	for _, headers := range [][]string{nil, {"basic", asAdmin}} {
		got := do(t, off.addr(), "DELETE", target, nil, headers...)
		got.header.Del("Date")
		if got.status != 404 || string(got.body) != `{"detail":"Not Found"}` || !reflect.DeepEqual(got.header, unknown.header) {
			t.Errorf("DELETE with %v: %d %v %q, want the 404 of a path no route takes %v", headers, got.status, got.header, got.body, unknown.header)
		}
	}
	for _, method := range []string{"PUT", "POST", "PATCH"} {
		got := do(t, off.addr(), method, target, nil, "basic", asAdmin)
		if got.status != 405 || got.header.Get("Allow") != "GET" {
			t.Errorf("%s: %d Allow %q %q, want 405 with Allow GET", method, got.status, got.header.Get("Allow"), got.body)
		}
	}
}
