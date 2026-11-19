package catalog_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// written are blobs of a store the write path wrote: a path of its own and a
// newer blob of a path of north.
var writtenSpecs = []testkit.BlobSpec{
	spec("demo-raw", "docs/written.txt", "written", t0+200),
	spec("demo-raw", "docs/tie.txt", "tie rewritten", t0+210),
	spec("demo-maven", "com/example/demo-lib/1.3.0/demo-lib-1.3.0.jar", "new jar", t0+220),
}

var formatOf = map[string]string{"demo-raw": "raw", "demo-maven": "maven2"}

func digestsOf(s testkit.BlobSpec) catalog.Digests {
	return catalog.Digests{MD5: strings.Repeat("a", 32), SHA1: testkit.SHA1Hex(s.Content), SHA256: strings.Repeat("b", 64),
		SHA512: strings.Repeat("c", 128)}
}

// assetOfSpec is the row the index takes for a blob of store.
func assetOfSpec(store string, s testkit.BlobSpec) index.Asset {
	sha1 := testkit.SHA1Hex(s.Content)
	return index.Asset{Repo: s.Repo, Path: s.Path, Store: store, Location: testkit.BytesLocation(s), Size: int64(len(s.Content)),
		SHA1: &sha1, ContentType: s.ContentType, CreatedMS: s.CreatedMS}
}

func put(t *testing.T, c *catalog.Catalog, store string, s testkit.BlobSpec) {
	t.Helper()
	if err := c.Put(assetOfSpec(store, s), formatOf[s.Repo], digestsOf(s), "demo-user", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
}

func open(t *testing.T, dir string) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// The rows Put writes for the blobs of the write path are the rows a scan of
// that store gives from its files, except the columns only a request knows:
// the digests other than SHA-1 and the last modified time. Those stay
// through the scan, as the blobs are the same.
func TestPutEqualsAScanOfTheFiles(t *testing.T) {
	f := newFixture(t)
	f.scan(f.north)
	write := makeStore(t, "write", filepath.Join(filepath.Dir(f.dir), "write"), writtenSpecs)
	c := open(t, f.dir)
	for _, s := range writtenSpecs {
		put(t, c, "write", s)
	}
	db := reader(t, f.dir)
	live := tableText(t, db)
	f.scan(write)
	if scanned := tableText(t, db); scanned != live {
		t.Fatalf("the rows of Put differ from a scan of the files\nPut\n%s\nscan\n%s", live, scanned)
	}
	f.equalsIndex(db, keysOf(northSpecs, writtenSpecs))
	for _, s := range writtenSpecs {
		k := key{s.Repo, s.Path}
		b := blobOf(t, db, "write", k)
		d := digestsOf(s)
		if b.md5.String != d.MD5 || b.sha256.String != d.SHA256 || b.sha512.String != d.SHA512 {
			t.Fatalf("%v: digests %v %v %v after the scan", k, b.md5, b.sha256, b.sha512)
		}
		if a := assetOf(t, db, k); a.store != "write" || a.lastModified != (sql.NullInt64{Int64: s.CreatedMS, Valid: true}) {
			t.Fatalf("%v: asset of %s, last modified %v; want write and %d", k, a.store, a.lastModified, s.CreatedMS)
		}
	}
	if a := assetOf(t, db, key{"demo-raw", "docs/readme.txt"}); a.lastModified.Valid {
		t.Fatalf("a scanned asset has a last modified time: %v", a.lastModified)
	}
}

// A blob older than the winning blob of its path keeps its row, and the
// asset stays with the newer blob and its times.
func TestPutOfAnOlderBlob(t *testing.T) {
	f := newFixture(t)
	f.scan(f.north)
	c := open(t, f.dir)
	old := spec("demo-raw", "docs/readme.txt", "older readme", t0)
	put(t, c, "write", old)
	db := reader(t, f.dir)
	k := key{"demo-raw", "docs/readme.txt"}
	if a := assetOf(t, db, k); a.store != "north" || a.lastModified.Valid {
		t.Fatalf("asset %+v, want that of north without a last modified time", a)
	}
	if b := blobOf(t, db, "write", k); b == nil || b.created != t0 {
		t.Fatalf("no row of the older blob: %+v", b)
	}
	if n := count(t, db, "SELECT blobs FROM scans WHERE store = 'write'"); n != 1 {
		t.Fatalf("scans counts %d blobs of the write store, want 1", n)
	}
}

// A second Put of a path replaces the row of its store: new digests, a new
// last modified time, the counts of the store stay one row.
func TestPutReplacesTheRowOfItsStore(t *testing.T) {
	f := newFixture(t)
	c := open(t, f.dir)
	first := spec("demo-raw", "docs/written.txt", "first", t0+200)
	second := spec("demo-raw", "docs/written.txt", "second, longer", t0+300)
	put(t, c, "write", first)
	put(t, c, "write", second)
	db := reader(t, f.dir)
	k := key{"demo-raw", "docs/written.txt"}
	b := blobOf(t, db, "write", k)
	if b.created != t0+300 || b.size != int64(len(second.Content)) || b.sha1.String != testkit.SHA1Hex(second.Content) {
		t.Fatalf("row %+v, want the second blob", b)
	}
	if a := assetOf(t, db, k); a.lastModified.Int64 != t0+300 {
		t.Fatalf("last modified %v, want %d", a.lastModified, t0+300)
	}
	var blobs, bytes int64
	if err := db.QueryRow("SELECT blobs, bytes FROM scans WHERE store = 'write'").Scan(&blobs, &bytes); err != nil {
		t.Fatal(err)
	}
	if blobs != 1 || bytes != int64(len(second.Content)) {
		t.Fatalf("scans %d blobs, %d bytes; want 1 and %d", blobs, bytes, len(second.Content))
	}
	if err := c.Put(index.Asset{Repo: "demo-raw", Path: "/docs/x.txt", Store: "write"}, "raw", catalog.Digests{}, "", ""); err == nil {
		t.Fatal("Put took a path with a leading slash")
	}
}

// Delete removes the asset of a path, the rows of every store for it and a
// component without assets; the counts of the stores follow.
func TestDeleteRemovesThePath(t *testing.T) {
	f := newFixture(t)
	c := open(t, f.dir)
	jar := writtenSpecs[2]
	put(t, c, "write", jar)
	put(t, c, "write", writtenSpecs[0])
	db := reader(t, f.dir)
	k := key{jar.Repo, jar.Path}
	if a := assetOf(t, db, k); a == nil || !a.component.Valid {
		t.Fatalf("no asset with a component after Put: %+v", a)
	}
	if err := c.Delete(jar.Repo, jar.Path); err != nil {
		t.Fatal(err)
	}
	if a := assetOf(t, db, k); a != nil {
		t.Fatalf("asset after Delete: %+v", a)
	}
	if b := blobOf(t, db, "write", k); b != nil {
		t.Fatalf("blob row after Delete: %+v", b)
	}
	if n := count(t, db, "SELECT count(*) FROM components WHERE repo = 'demo-maven'"); n != 0 {
		t.Fatalf("%d components of demo-maven after its only asset went", n)
	}
	if n := count(t, db, "SELECT blobs FROM scans WHERE store = 'write'"); n != 1 {
		t.Fatalf("scans counts %d blobs of the write store, want 1", n)
	}
	if err := c.Delete("demo-raw", "docs/none.txt"); err != nil {
		t.Fatalf("Delete of a path without rows: %v", err)
	}
}

// Drop removes the row of one store for a path, as index.Drop does in the
// index, and the asset follows the row that remains.
func TestDropFollowsTheIndex(t *testing.T) {
	f := newFixture(t)
	f.scan(f.north)
	f.scan(f.south)
	c := open(t, f.dir)
	k := key{"demo-raw", "docs/readme.txt"}
	if err := f.ix.Drop(k.repo, k.path, "south"); err != nil {
		t.Fatal(err)
	}
	if err := c.Drop(k.repo, k.path, "south"); err != nil {
		t.Fatal(err)
	}
	db := reader(t, f.dir)
	f.equalsIndex(db, keysOf(northSpecs, southSpecs))
	if a := assetOf(t, db, k); a == nil || a.store != "north" {
		t.Fatalf("asset after the drop of the south row: %+v", a)
	}
	only := key{"demo-raw", "docs/south-only.txt"}
	if err := f.ix.Drop(only.repo, only.path, "south"); err != nil {
		t.Fatal(err)
	}
	if err := c.Drop(only.repo, only.path, "south"); err != nil {
		t.Fatal(err)
	}
	f.equalsIndex(db, keysOf(northSpecs, southSpecs))
	if a := assetOf(t, db, only); a != nil {
		t.Fatalf("asset of a path no store holds: %+v", a)
	}
}
