package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

type file struct {
	name string
	data []byte
	typ  byte
}

// tgz writes the files as a gzip tar archive with the writers of the
// standard library, as npm pack would.
func tgz(t *testing.T, files ...file) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, f := range files {
		typ := f.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data)), Typeflag: typ, Format: tar.FormatUSTAR}
		if typ == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ != tar.TypeDir {
			if _, err := tw.Write(f.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz(t, raw.Bytes())
}

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func manifestJSON(version string) []byte {
	return []byte(`{"name": "demo-lib", "version": "` + version + `", "main": "index.js"}`)
}

func decodeObject(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.DecodeString(text)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

func TestParsePathNative(t *testing.T) {
	for path, want := range map[string]PackagePath{
		"demo-lib":         {Name: "demo-lib"},
		"@demo/util":       {Name: "@demo/util"},
		"demo-lib/1.0.0":   {Name: "demo-lib", Version: "1.0.0"},
		"@demo/util/2.0.0": {Name: "@demo/util", Version: "2.0.0"},
	} {
		if got, ok := ParsePath(path); !ok || got != want {
			t.Errorf("ParsePath(%q) = %+v, %v, want %+v", path, got, ok, want)
		}
	}
	for _, path := range []string{"", "-/ping", "@demo", "demo-lib/-/demo-lib-1.0.0.tgz", "@demo/..", "a/" + strings.Repeat("1", 129)} {
		if got, ok := ParsePath(path); ok {
			t.Errorf("ParsePath(%q) = %+v, want None", path, got)
		}
	}
	if _, ok := PackageName("demo-lib/1.0.0"); ok {
		t.Error("a version path has no package name")
	}
}

func TestDocumentFromTarballsNative(t *testing.T) {
	first := tgz(t, file{name: "package/package.json", data: manifestJSON("1.1.0")}, file{name: "package/index.js", data: []byte("x\n")})
	second := tgz(t, file{name: "package/package.json", data: manifestJSON("1.0.0")})
	beta := tgz(t, file{name: "package/package.json", data: manifestJSON("2.0.0-beta.1")})
	doc, err := DocumentFromTarballs("demo-lib", []Tarball{
		{FileName: "demo-lib-1.1.0.tgz", Data: first, CreatedMS: 1_700_200_000_000},
		{FileName: "demo-lib-1.0.0.tgz", Data: second, CreatedMS: 1_700_000_000_000},
		{FileName: "demo-lib-2.0.0-beta.1.tgz", Data: beta, CreatedMS: 1_700_300_000_000},
		{FileName: "broken.tgz", Data: []byte("not a tarball"), CreatedMS: 1},
	})
	if err != nil || doc == nil {
		t.Fatalf("DocumentFromTarballs: %v, %v", doc, err)
	}
	if got := doc.Keys(); strings.Join(got, ",") != "name,dist-tags,versions,time" {
		t.Errorf("keys %v", got)
	}
	versions, _ := doc.Get("versions")
	if got := strings.Join(versions.(*pyjson.Object).Keys(), ","); got != "1.0.0,1.1.0,2.0.0-beta.1" {
		t.Errorf("versions in order %s", got)
	}
	tags, _ := doc.Get("dist-tags")
	if latest, _ := tags.(*pyjson.Object).Get("latest"); latest != "1.1.0" {
		t.Errorf("latest %v, want the highest release", latest)
	}
	entry, _ := versions.(*pyjson.Object).Get("1.1.0")
	dist, _ := entry.(*pyjson.Object).Get("dist")
	sum := sha1.Sum(first)
	long := sha512.Sum512(first)
	wantDist := `{"tarball": "demo-lib/-/demo-lib-1.1.0.tgz", "shasum": "` + hex.EncodeToString(sum[:]) +
		`", "integrity": "sha512-` + base64.StdEncoding.EncodeToString(long[:]) + `"}`
	if got := string(dump(t, dist)); got != wantDist {
		t.Errorf("dist %s, want %s", got, wantDist)
	}
	times, _ := doc.Get("time")
	if got := string(dump(t, times)); got != `{"created": "2023-11-14T22:13:20.000Z", "modified": "2023-11-18T09:33:20.000Z", `+
		`"1.0.0": "2023-11-14T22:13:20.000Z", "1.1.0": "2023-11-17T05:46:40.000Z", "2.0.0-beta.1": "2023-11-18T09:33:20.000Z"}` {
		t.Errorf("time %s", got)
	}
	if doc, err := DocumentFromTarballs("demo-lib", nil); doc != nil || err != nil {
		t.Errorf("no tarballs: %v, %v", doc, err)
	}
}

func TestPackageManifestBounds(t *testing.T) {
	body := manifestJSON("1.0.0")
	atLimit := append(append([]byte(nil), body...), bytes.Repeat([]byte(" "), MaxPackageJSONBytes-len(body))...)
	if m, err := PackageManifest(tgz(t, file{name: PackageJSON, data: atLimit})); m == nil || err != nil {
		t.Errorf("a manifest of exactly %d bytes is read: %v, %v", MaxPackageJSONBytes, m, err)
	}
	over := append(atLimit, ' ')
	if m, err := PackageManifest(tgz(t, file{name: PackageJSON, data: over})); m != nil || err != nil {
		t.Errorf("a manifest of %d bytes is left out: %v, %v", len(over), m, err)
	}
	// the last member of the name wins, as tarfile.getmember answers
	two := tgz(t, file{name: PackageJSON, data: manifestJSON("1.0.0")}, file{name: PackageJSON, data: manifestJSON("2.0.0")})
	m, err := PackageManifest(two)
	if err != nil || m == nil {
		t.Fatalf("two manifests: %v, %v", m, err)
	}
	if version, _ := m.Get("version"); version != "2.0.0" {
		t.Errorf("version %v, want the last member's", version)
	}
	dir := tgz(t, file{name: PackageJSON, data: manifestJSON("1.0.0")}, file{name: PackageJSON + "/", typ: tar.TypeDir})
	if m, err := PackageManifest(dir); m != nil || err != nil {
		t.Errorf("a directory as last member: %v, %v", m, err)
	}
}

// chain is an archive whose manifest follows count extended headers.
func chain(t *testing.T, count int) []byte {
	t.Helper()
	pad := func(b []byte) []byte { return append(b, make([]byte, (512-len(b)%512)%512)...) }
	header := func(name string, size int, typ byte) []byte {
		var raw bytes.Buffer
		tw := tar.NewWriter(&raw)
		_ = tw.WriteHeader(&tar.Header{Name: name, Size: int64(size), Mode: 0o644, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
		h := raw.Bytes()[:512]
		h[156] = typ
		var sum int64
		copy(h[148:156], "        ")
		for _, c := range h {
			sum += int64(c)
		}
		copy(h[148:156], []byte(strings.Repeat("0", 6-len(itoa8(sum)))+itoa8(sum)+"\x00 "))
		return h
	}
	record := []byte("12 comment=\n")
	var raw []byte
	for range count {
		raw = append(raw, header("././@PaxHeader", len(record), 'x')...)
		raw = append(raw, pad(append([]byte(nil), record...))...)
	}
	body := manifestJSON("1.0.0")
	raw = append(raw, header(PackageJSON, len(body), '0')...)
	raw = append(raw, pad(append([]byte(nil), body...))...)
	raw = append(raw, make([]byte, 1024)...)
	return gz(t, raw)
}

func itoa8(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 8 {
		b = append([]byte{byte('0' + n%8)}, b...)
	}
	return string(b)
}

func TestPackageManifestHeaderChain(t *testing.T) {
	for count, found := range map[int]bool{0: true, 1: true, maxHeaderChain: true, maxHeaderChain + 1: false} {
		m, err := PackageManifest(chain(t, count))
		if err != nil || (m != nil) != found {
			t.Errorf("a chain of %d extended headers: %v, %v, want found %v", count, m, err, found)
		}
	}
}

func TestPackageManifestUncaught(t *testing.T) {
	// A GNU sparse header that says an extension block follows, at the end
	// of a gzip stream that ends cleanly: Python's tarfile indexes the short
	// block and raises IndexError, which package_manifest does not catch.
	h := make([]byte, 512)
	copy(h, PackageJSON)
	copy(h[100:], "0000644\x00")
	copy(h[124:], "00000000000\x00")
	h[156] = 'S'
	copy(h[257:], "ustar  \x00")
	h[482] = 1
	copy(h[483:], "00000000000\x00")
	copy(h[148:156], "        ")
	var sum int64
	for _, c := range h {
		sum += int64(c)
	}
	copy(h[148:156], []byte(strings.Repeat("0", 6-len(itoa8(sum)))+itoa8(sum)+"\x00 "))
	m, err := PackageManifest(gz(t, h))
	var pyErr *pyjson.Error
	if m != nil || !errors.As(err, &pyErr) || pyErr.Class != "IndexError" {
		t.Errorf("short sparse extension: %v, %v", m, err)
	}
}

func TestMergeDocumentsCopies(t *testing.T) {
	one := decodeObject(t, `{"name": "demo-lib", "_rev": "1", "versions": {"1.0.0": {"dist": {"shasum": "a"}}, "1.1.0": {"dist": {"shasum": "b"}}}, "dist-tags": {"latest": "1.1.0"}}`)
	two := decodeObject(t, `{"name": "demo-lib", "_rev": "2", "versions": {"1.1.0": {"dist": {"shasum": "c"}}, "1.2.0": {"dist": {"shasum": "d"}}}, "time": {"1.2.0": "t"}}`)
	before := [][]byte{dump(t, one), dump(t, two)}
	merged := MergeDocuments([]*pyjson.Object{one, two})
	want := `{"name": "demo-lib", "_rev": "1", "versions": {"1.0.0": {"dist": {"shasum": "a"}}, "1.1.0": {"dist": {"shasum": "b"}}, ` +
		`"1.2.0": {"dist": {"shasum": "d"}}}, "dist-tags": {"latest": "1.1.0"}, "time": {"1.2.0": "t"}}`
	if got := string(dump(t, merged)); got != want {
		t.Errorf("merged %s\nwant %s", got, want)
	}
	// the result shares nothing with the inputs
	versions, _ := merged.Get("versions")
	entry, _ := versions.(*pyjson.Object).Get("1.2.0")
	dist, _ := entry.(*pyjson.Object).Get("dist")
	dist.(*pyjson.Object).Set("shasum", "changed")
	versions.(*pyjson.Object).Set("9.9.9", "x")
	if !bytes.Equal(dump(t, one), before[0]) || !bytes.Equal(dump(t, two), before[1]) {
		t.Error("a change of the merge result reached an input")
	}
	if MergeDocuments(nil) != nil {
		t.Error("an empty list merges to None")
	}
}

func TestRewriteTarballsNative(t *testing.T) {
	doc := decodeObject(t, `{"name": "@demo/util", "versions": {"2.0.0": {"dist": {"tarball": "https://old.example/r/@demo/util/-/util-2.0.0%2Bb1.tgz", "shasum": "a"}}, "2.1.0": {}}}`)
	before := dump(t, doc)
	out, err := RewriteTarballs(doc, "http://localhost:14193/repository/demo-npm-group/")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name": "@demo/util", "versions": {"2.0.0": {"dist": {"tarball": "http://localhost:14193/repository/demo-npm-group/@demo/util/-/util-2.0.0%2Bb1.tgz", "shasum": "a"}}, ` +
		`"2.1.0": {"dist": {"tarball": "http://localhost:14193/repository/demo-npm-group/@demo/util/-/util-2.1.0.tgz"}}}}`
	if got := string(dump(t, out)); got != want {
		t.Errorf("rewritten %s\nwant %s", got, want)
	}
	if !bytes.Equal(dump(t, doc), before) {
		t.Error("RewriteTarballs changed its input")
	}
}

func TestSelectVersionsNative(t *testing.T) {
	doc := decodeObject(t, `{"name": "demo-lib", "versions": {"1.0.0": {}, "1.1.0": {}, "2.0.0-rc.1": {}}, "time": {"created": "c", "1.1.0": "t"}, "dist-tags": {"latest": "1.1.0", "next": "2.0.0-rc.1"}}`)
	out, err := SelectVersions(doc, []string{"1.0.0", "2.0.0-rc.1"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name": "demo-lib", "versions": {"1.0.0": {}, "2.0.0-rc.1": {}}, "time": {"created": "c"}, "dist-tags": {"next": "2.0.0-rc.1", "latest": "1.0.0"}}`
	if got := string(dump(t, out)); got != want {
		t.Errorf("selected %s\nwant %s", got, want)
	}
	if out, err := SelectVersions(doc, nil); out != nil || err != nil {
		t.Errorf("nothing kept: %v, %v", out, err)
	}
}
