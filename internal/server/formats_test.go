package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/testkit"
)

const libMetadata = "com/example/demo-lib/maven-metadata.xml"

func metadataSpec(repo string, versions []string, lastUpdated string, created int64, pad int) testkit.BlobSpec {
	data := testkit.MetadataXML("com.example", "demo-lib", versions, lastUpdated)
	data = append(data, []byte("<!--"+strings.Repeat("p", pad)+"-->\n")...)
	s := spec(repo, libMetadata, data, "application/xml")
	s.CreatedMS = created
	return s
}

func groupDefs() []*pyjson.Object {
	return []*pyjson.Object{def("demo-releases", "maven2", "hosted"), def("demo-snapshots", "maven2", "hosted"),
		def("demo-group", "maven2", "group", "demo-releases", "demo-snapshots")}
}

func TestGroupMetadataSkipsLargeDocuments(t *testing.T) {
	defer func(old int64) { maxMetadataBytes = old }(maxMetadataBytes)
	maxMetadataBytes = 1000
	base := t.TempDir()
	small := metadataSpec("demo-releases", []string{"1.0"}, "20240101000000", createdMS, 0)
	large := metadataSpec("demo-snapshots", []string{"2.0"}, "20240301000000", createdMS+5000, 1000)
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", small, large)}, appOptions{defs: groupDefs()})
	r := a.get("/repository/demo-group/" + libMetadata)
	expect(t, r, 200)
	if !strings.Contains(string(r.body), "<version>1.0</version>") || strings.Contains(string(r.body), "2.0") {
		t.Fatalf("the large document took part: %s", r.body)
	}
	if r.header.Get("Last-Modified") != "Tue, 14 Nov 2023 22:13:20 GMT" {
		t.Fatalf("the skipped document counts for the date: %s", r)
	}
	// exactly at the bound the document is read
	maxMetadataBytes = int64(len(large.Content))
	if r := a.get("/repository/demo-group/" + libMetadata); !strings.Contains(string(r.body), "2.0") {
		t.Fatalf("a document of exactly the bound is skipped: %s", r.body)
	}
	maxMetadataBytes = 10
	expect(t, a.get("/repository/demo-group/"+libMetadata), 404)
	expect(t, a.get("/repository/demo-group/"+libMetadata+".sha1"), 404)
}

func TestGroupMetadataOfUnavailableMember(t *testing.T) {
	base := t.TempDir()
	s1 := blobStore(t, base, "s1", metadataSpec("demo-releases", []string{"1.0"}, "20240101000000", createdMS, 0))
	s2 := blobStore(t, base, "s2", metadataSpec("demo-snapshots", []string{"2.0"}, "20240301000000", createdMS, 0))
	a := newApp(t, []config.StoreConfig{s1, s2}, appOptions{defs: groupDefs()})
	expect(t, a.get("/repository/demo-group/"+libMetadata), 200)
	if err := os.RemoveAll(s2.Root); err != nil {
		t.Fatal(err)
	}
	r := a.get("/repository/demo-group/" + libMetadata + ".md5")
	expect(t, r, 503)
	if string(r.body) != "Store s2 is not available\n" || r.header.Get("Content-Type") != textType {
		t.Fatalf("%s", r)
	}
}

// tarballOf is a gzip tar with package/package.json of name and version.
func tarballOf(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	manifest := []byte(fmt.Sprintf(`{"name": %q, "version": %q}`, name, version))
	if err := tw.WriteHeader(&tar.Header{Name: "package/package.json", Mode: 0o644, Size: int64(len(manifest)),
		Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(manifest); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func npmVersions(t *testing.T, data []byte) string {
	t.Helper()
	value, err := pyjson.Decode(data)
	if err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	versions, _ := value.(*pyjson.Object).Get("versions")
	return strings.Join(versions.(*pyjson.Object).Keys(), " ")
}

func TestNpmReadLimits(t *testing.T) {
	defer func(d, tb, p int64) { maxDocumentBytes, maxTarballBytes, maxPackageBytes = d, tb, p }(
		maxDocumentBytes, maxTarballBytes, maxPackageBytes)
	base := t.TempDir()
	one, two, three := tarballOf(t, "demo-lib", "1.0.0"), tarballOf(t, "demo-lib", "2.0.0"), tarballOf(t, "demo-lib", "3.0.0")
	document := []byte(`{"name": "demo-lib", "versions": {"1.0.0": {"name": "demo-lib", "version": "1.0.0", "description": "from the document"}}}`)
	blobs := []testkit.BlobSpec{
		spec("demo-npm", "demo-lib", document, "application/json"),
		spec("demo-npm", "demo-lib/-/demo-lib-1.0.0.tgz", one, ""),
		spec("demo-npm", "demo-lib/-/demo-lib-2.0.0.tgz", two, ""),
		spec("demo-npm", "demo-lib/-/demo-lib-3.0.0.tgz", three, ""),
	}
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", blobs...)}, appOptions{defs: []*pyjson.Object{def("demo-npm", "npm", "hosted")}})
	get := func() *response {
		t.Helper()
		r := a.get("/repository/demo-npm/demo-lib", "Host", "localhost:14193")
		expect(t, r, 200)
		return r
	}
	if r := get(); npmVersions(t, r.body) != "1.0.0 2.0.0 3.0.0" || !strings.Contains(string(r.body), "from the document") {
		t.Fatalf("%s", r.body)
	}
	maxDocumentBytes = int64(len(document)) - 1
	if r := get(); npmVersions(t, r.body) != "1.0.0 2.0.0 3.0.0" || strings.Contains(string(r.body), "from the document") {
		t.Fatalf("a document above the bound is read: %s", r.body)
	}
	maxDocumentBytes = int64(len(document))
	if r := get(); !strings.Contains(string(r.body), "from the document") {
		t.Fatalf("a document of exactly the bound is skipped: %s", r.body)
	}
	maxTarballBytes = int64(min(len(two), len(three))) - 1
	if r := get(); npmVersions(t, r.body) != "1.0.0" {
		t.Fatalf("a tarball above the bound is read: %s", r.body)
	}
	maxTarballBytes = int64(max(len(two), len(three)))
	maxPackageBytes = int64(len(two)) - 1
	if r := get(); npmVersions(t, r.body) != "1.0.0 2.0.0" {
		t.Fatalf("the reading does not stop past the package bound: %s", r.body)
	}
	maxPackageBytes = int64(len(two) + len(three))
	if r := get(); npmVersions(t, r.body) != "1.0.0 2.0.0 3.0.0" {
		t.Fatalf("the reading stops at the package bound: %s", r.body)
	}
}

func TestNpmOfUnavailableStore(t *testing.T) {
	base := t.TempDir()
	s1 := blobStore(t, base, "s1", spec("demo-npm", "demo-lib/-/demo-lib-1.0.0.tgz", tarballOf(t, "demo-lib", "1.0.0"), ""))
	a := newApp(t, []config.StoreConfig{s1}, appOptions{defs: []*pyjson.Object{def("demo-npm", "npm", "hosted")}})
	expect(t, a.get("/repository/demo-npm/demo-lib"), 200)
	if err := os.RemoveAll(s1.Root); err != nil {
		t.Fatal(err)
	}
	r := a.get("/repository/demo-npm/demo-lib")
	expect(t, r, 503)
	if string(r.body) != "Store s1 is not available\n" {
		t.Fatalf("%s", r)
	}
}

func TestListingPages(t *testing.T) {
	defer func(limit, page int) { listingLimit, listingPage = limit, page }(listingLimit, listingPage)
	listingLimit, listingPage = 2, 1
	base := t.TempDir()
	var blobs []testkit.BlobSpec
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		blobs = append(blobs, spec("demo-raw", "docs/"+name+".txt", []byte(name), ""))
	}
	blobs = append(blobs, spec("demo-raw2", "docs/b.txt", []byte("other"), ""), spec("demo-raw2", "docs/f.txt", []byte("f"), ""))
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", blobs...)}, appOptions{defs: []*pyjson.Object{
		def("demo-raw", "raw", "hosted"), def("demo-raw2", "raw", "hosted"), def("demo-raw-group", "raw", "group", "demo-raw", "demo-raw2")}})
	more := func(next int) string {
		return fmt.Sprintf(`<p class="pager">More entries follow. <a href="?offset=%d">Next page</a></p>`, next)
	}
	from := func(first int) string {
		return fmt.Sprintf(`<p class="pager">Entries from number %d. <a href="./">First page</a></p>`, first)
	}
	for _, c := range []struct {
		target string
		names  []string
		pager  []string
		absent []string
	}{
		{"/repository/demo-raw/docs/", []string{"a.txt", "b.txt"}, []string{more(2)}, []string{"c.txt", "First page"}},
		{"/repository/demo-raw/docs/?offset=2", []string{"c.txt", "d.txt"}, []string{from(3), more(4)}, []string{"b.txt", "e.txt"}},
		{"/repository/demo-raw/docs/?offset=4", []string{"e.txt"}, []string{from(5)}, []string{"Next page"}},
		{"/repository/demo-raw/docs/?offset=5", nil, []string{from(6)}, []string{"Next page", ".txt"}},
		{"/repository/demo-raw-group/docs/?offset=1", []string{"b.txt", "c.txt"}, []string{from(2), more(3)}, []string{"a.txt"}},
		{"/repository/demo-raw-group/docs/?offset=4", []string{"e.txt", "f.txt"}, []string{from(5)}, []string{"Next page"}},
	} {
		r := a.get(c.target)
		expect(t, r, 200)
		page := string(r.body)
		for _, name := range c.names {
			if !strings.Contains(page, ">"+name+"</a>") {
				t.Errorf("%s: %s is missing", c.target, name)
			}
		}
		for _, p := range c.pager {
			if !strings.Contains(page, p) {
				t.Errorf("%s: pager %q is missing", c.target, p)
			}
		}
		for _, s := range c.absent {
			if strings.Contains(page, s) {
				t.Errorf("%s: %q is there", c.target, s)
			}
		}
	}
	// the group's b.txt is the one of the first member
	if r := a.get("/repository/demo-raw-group/docs/?offset=1"); strings.Contains(string(r.body), `<td class="size">5</td>`) {
		t.Errorf("the first member does not win: %s", r.body)
	}
}

// TestNpmSurrogateIsAServerError: sqlite3 of Python cannot bind a str with
// a lone surrogate, so the lookup of such a tarball path raises
// UnicodeEncodeError and the answer is 500 (the cases of the scenario
// surrogate in server_formats.json). The log names the characters.
func TestNpmSurrogateIsAServerError(t *testing.T) {
	logs := captureLog(t)
	base := t.TempDir()
	document := []byte(`{"versions": {"1.0.0": {"dist": {"tarball": "http://h/x\udfff\ud800.tgz"}}}}`)
	blobs := []testkit.BlobSpec{
		spec("demo-npm", "demo-lib", document, "application/json"),
		spec("demo-npm", "demo-lib/-/demo-lib-1.0.0.tgz", tarballOf(t, "demo-lib", "1.0.0"), ""),
	}
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", blobs...)}, appOptions{defs: []*pyjson.Object{def("demo-npm", "npm", "hosted")}})
	r := a.get("/repository/demo-npm/demo-lib")
	if r.status != 500 || string(r.body) != "Internal Server Error" {
		t.Fatalf("%s", r)
	}
	want := "Exception in ASGI application: UnicodeEncodeError: 'utf-8' codec can't encode characters in position 1-2: surrogates not allowed"
	for _, line := range logs.all() {
		if strings.HasSuffix(line, want) {
			return
		}
	}
	t.Fatalf("no log line %q in %q", want, logs.all())
}
