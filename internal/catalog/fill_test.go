package catalog_test

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

const t0 = int64(1_700_000_000_000)

const blobDigest = "sha256:6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eebdaab7c610cf7d66709b3b"

type key struct{ repo, path string }

func spec(repo, path, content string, created int64) testkit.BlobSpec {
	s := testkit.NewBlobSpec(repo, path)
	s.Content, s.CreatedMS = []byte(content), created
	return s
}

// The invented fixture: two stores, a path in both with a newer blob in
// south, a path in both with the same creation time, a path twice in north,
// formats from repositories.json, a repository only the hint of the index
// names and one that neither names.
var (
	northSpecs = []testkit.BlobSpec{
		spec("demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar", "jar bytes", t0),
		spec("demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.pom", "pom bytes", t0+1),
		spec("demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar.sha1", "sha1 text", t0+2),
		spec("demo-maven", "com/example/demo-lib/maven-metadata.xml", "metadata", t0+3),
		spec("demo-npm", "demo-pkg/-/demo-pkg-1.0.0.tgz", "tarball", t0+4),
		spec("demo-npm", "demo-pkg", "document", t0+5),
		spec("demo-docker", "v2/demo/app/manifests/1.4.2", "manifest", t0+6),
		spec("demo-docker", "v2/-/blobs/"+blobDigest, "layer", t0+7),
		spec("demo-raw", "docs/readme.txt", "old readme", t0+8),
		spec("demo-raw", "docs/tie.txt", "tie north", t0+10),
		spec("demo-raw", "docs/twice.txt", "first", t0+20),
		spec("demo-raw", "docs/twice.txt", "second, newer", t0+30),
		spec("demo-raw", "docs/twice-tie.txt", "tie a", t0+40),
		spec("demo-raw", "docs/twice-tie.txt", "tie b", t0+40),
		spec("demo-raw", "docs/anonymous.txt", "no uploader", t0+50),
		spec("demo-hinted", "com/example/hinted/1.0/hinted-1.0.pom", "hinted pom", t0+60),
		spec("demo-plain", "notes/plain.txt", "plain", t0+70),
	}
	southSpecs = []testkit.BlobSpec{
		spec("demo-raw", "docs/readme.txt", "new readme", t0+100),
		spec("demo-raw", "docs/tie.txt", "tie south", t0+10),
		spec("demo-raw", "docs/south-only.txt", "south only", t0+110),
	}
	definitions = `{"repositories": [{"name": "demo-maven", "format": "maven2", "type": "hosted"},` +
		` {"name": "demo-npm", "format": "npm", "type": "hosted"}, {"name": "demo-docker", "format": "docker", "type": "hosted"},` +
		` {"name": "demo-raw", "format": "raw", "type": "hosted"}]}`
)

// fixture is a data directory with the two stores and their index.
type fixture struct {
	t            *testing.T
	dir          string
	ix           *index.Index
	north, south stores.Store
}

func makeStore(t *testing.T, name, root string, specs []testkit.BlobSpec) stores.Store {
	t.Helper()
	if _, err := testkit.MakeBlobStore(root, specs); err != nil {
		t.Fatal(err)
	}
	s, err := stores.NewBlobStore(name, root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{t: t, dir: filepath.Join(base, "data")}
	if err := os.MkdirAll(f.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "repositories.json"), []byte(definitions), 0o600); err != nil {
		t.Fatal(err)
	}
	f.north = makeStore(t, "north", filepath.Join(base, "north"), northSpecs)
	f.south = makeStore(t, "south", filepath.Join(base, "south"), southSpecs)
	// the blob of docs/anonymous.txt names no uploader
	anonymous := northSpecs[14]
	items := testkit.SpecProperties(anonymous)
	delete(items, "@BlobStore.created-by")
	delete(items, "@BlobStore.created-by-ip")
	properties := filepath.Join(f.north.Root(), filepath.FromSlash(testkit.PropertiesLocation(anonymous)))
	if err := os.WriteFile(properties, testkit.EncodeProperties(items, anonymous.CreatedMS), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	if f.ix, err = index.Open(filepath.Join(f.dir, "index.sqlite")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.ix.Close() })
	return f
}

func (f *fixture) scan(s stores.Store) scan.Result {
	f.t.Helper()
	result, err := scan.Run(s, f.ix, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

// fileDSN is a file: URI of path, so that any character reaches SQLite.
func fileDSN(t *testing.T, path, query string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + query
}

// reader opens the catalog file read only, as another process would.
func reader(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", fileDSN(t, catalog.Path(dir), "?mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type blobRow struct {
	size, created                     int64
	sha1, contentType, uploader, addr sql.NullString
	md5, sha256, sha512               sql.NullString
}

type assetRow struct {
	store, format                string
	component                    sql.NullInt64
	extension, classifier        sql.NullString
	lastModified, lastDownloaded sql.NullInt64
	namespace, name, version     sql.NullString
	prerelease                   sql.NullBool
}

func blobOf(t *testing.T, db *sql.DB, store string, k key) *blobRow {
	t.Helper()
	var b blobRow
	err := db.QueryRow("SELECT size, sha1, content_type, created_ms, uploader, uploader_ip, md5, sha256, sha512 FROM blobs"+
		" WHERE store = ? AND repo = ? AND path = ?", store, k.repo, k.path).Scan(&b.size, &b.sha1, &b.contentType,
		&b.created, &b.uploader, &b.addr, &b.md5, &b.sha256, &b.sha512)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &b
}

func assetOf(t *testing.T, db *sql.DB, k key) *assetRow {
	t.Helper()
	var a assetRow
	err := db.QueryRow("SELECT a.store, a.format, a.component, a.extension, a.classifier, a.last_modified_ms,"+
		" a.last_downloaded_ms, c.namespace, c.name, c.version, c.prerelease FROM assets a"+
		" LEFT JOIN components c ON c.id = a.component WHERE a.repo = ? AND a.path = ?", k.repo, k.path).Scan(
		&a.store, &a.format, &a.component, &a.extension, &a.classifier, &a.lastModified, &a.lastDownloaded,
		&a.namespace, &a.name, &a.version, &a.prerelease)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &a
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func nullable(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

func keysOf(specs ...[]testkit.BlobSpec) []key {
	var keys []key
	for _, list := range specs {
		for _, s := range list {
			if k := (key{s.Repo, s.Path}); !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// equalsIndex checks every key against index.Lookup: the asset row names the
// store of the winning row, and the blob row of that store has its size,
// SHA-1, content type and creation time. Counts per repository equal
// RepoStats, rows per store equal StoreStats and the rows of scans.
func (f *fixture) equalsIndex(db *sql.DB, keys []key) {
	t := f.t
	t.Helper()
	for _, k := range keys {
		want, err := f.ix.Lookup(k.repo, k.path)
		if err != nil {
			t.Fatal(err)
		}
		a := assetOf(t, db, k)
		switch {
		case want == nil && a != nil:
			t.Fatalf("%v: the index has no row, the catalog an asset of %s", k, a.store)
		case want == nil:
			continue
		case a == nil:
			t.Fatalf("%v: no asset, the index has a row of %s", k, want.Store)
		case a.store != want.Store:
			t.Fatalf("%v: asset of store %s, the index row is of %s", k, a.store, want.Store)
		}
		b := blobOf(t, db, a.store, k)
		if b == nil || b.size != want.Size || b.sha1 != nullable(want.SHA1) || b.contentType != nullable(want.ContentType) ||
			b.created != want.CreatedMS {
			t.Fatalf("%v: blob %+v, the index row %+v", k, b, *want)
		}
	}
	stats, err := f.ix.RepoStats()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stats {
		files := count(t, db, "SELECT count(*) FROM assets WHERE repo = ?", s.Repo)
		bytes := count(t, db, "SELECT coalesce(sum(b.size), 0) FROM assets a JOIN blobs b ON b.store = a.store"+
			" AND b.repo = a.repo AND b.path = a.path WHERE a.repo = ?", s.Repo)
		if files != s.Files || bytes != s.Bytes {
			t.Fatalf("repository %s: %d assets of %d bytes, the index counts %d of %d", s.Repo, files, bytes, s.Files, s.Bytes)
		}
	}
	if repos := count(t, db, "SELECT count(DISTINCT repo) FROM assets"); repos != int64(len(stats)) {
		t.Fatalf("assets of %d repositories, the index has %d", repos, len(stats))
	}
	storeStats, err := f.ix.StoreStats()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range storeStats {
		rows := count(t, db, "SELECT count(*) FROM blobs WHERE store = ?", s.Store)
		bytes := count(t, db, "SELECT coalesce(sum(size), 0) FROM blobs WHERE store = ?", s.Store)
		var scanned struct{ blobs, bytes int64 }
		if err := db.QueryRow("SELECT blobs, bytes FROM scans WHERE store = ?", s.Store).Scan(&scanned.blobs, &scanned.bytes); err != nil {
			t.Fatalf("scans row of %s: %v", s.Store, err)
		}
		if rows != s.Files || bytes != s.Bytes || scanned.blobs != s.Files || scanned.bytes != s.Bytes {
			t.Fatalf("store %s: %d blobs of %d bytes, scans %+v, the index %d of %d", s.Store, rows, bytes, scanned, s.Files, s.Bytes)
		}
	}
	if n := count(t, db, "SELECT count(*) FROM scans"); n != int64(len(storeStats)) {
		t.Fatalf("%d rows in scans, the index has %d stores", n, len(storeStats))
	}
}

// A scan of two stores fills the catalog: every asset equals the winning row
// of the index, uploader and address are those of the properties files, and
// the coordinates follow the format of the repository.
func TestScanFillsTheCatalog(t *testing.T) {
	f := newFixture(t)
	if _, err := os.Stat(catalog.Path(f.dir)); err == nil {
		t.Fatal("a catalog before the first scan")
	}
	f.scan(f.north)
	f.scan(f.south)
	db := reader(t, f.dir)
	f.equalsIndex(db, keysOf(northSpecs, southSpecs))

	readme := assetOf(t, db, key{"demo-raw", "docs/readme.txt"})
	tie := assetOf(t, db, key{"demo-raw", "docs/tie.txt"})
	if readme.store != "south" || tie.store != "north" {
		t.Fatalf("readme from %s, tie from %s; want south (newer) and north (name order on a tie)", readme.store, tie.store)
	}
	for _, k := range []key{{"demo-raw", "docs/readme.txt"}, {"demo-raw", "docs/tie.txt"}} {
		if blobOf(t, db, "north", k) == nil || blobOf(t, db, "south", k) == nil {
			t.Fatalf("%v: a blob row per store expected", k)
		}
	}
	user, address := sql.NullString{String: "demo-user", Valid: true}, sql.NullString{String: "192.0.2.10", Valid: true}
	for _, k := range keysOf(northSpecs) {
		b := blobOf(t, db, "north", k)
		wantUser, wantAddress := user, address
		if k.path == "docs/anonymous.txt" {
			wantUser, wantAddress = sql.NullString{}, sql.NullString{}
		}
		if b.uploader != wantUser || b.addr != wantAddress || b.md5.Valid || b.sha256.Valid || b.sha512.Valid {
			t.Fatalf("%v: uploader %v, address %v, digests %v %v %v", k, b.uploader, b.addr, b.md5, b.sha256, b.sha512)
		}
	}

	type coordinates struct {
		format, namespace, name, version, extension string
		component, prerelease                       bool
	}
	for k, want := range map[key]coordinates{
		{"demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar"}:      {"maven2", "com.example", "demo-lib", "1.2.0", "jar", true, false},
		{"demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.pom"}:      {"maven2", "com.example", "demo-lib", "1.2.0", "pom", true, false},
		{"demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar.sha1"}: {"maven2", "com.example", "demo-lib", "1.2.0", "jar.sha1", true, false},
		{"demo-maven", "com/example/demo-lib/maven-metadata.xml"}:            {format: "maven2"},
		{"demo-npm", "demo-pkg/-/demo-pkg-1.0.0.tgz"}:                        {"npm", "", "demo-pkg", "1.0.0", "", true, false},
		{"demo-npm", "demo-pkg"}:                                             {"npm", "", "demo-pkg", "", "", true, false},
		{"demo-docker", "v2/demo/app/manifests/1.4.2"}:                       {"docker", "", "demo/app", "1.4.2", "", true, false},
		{"demo-docker", "v2/-/blobs/" + blobDigest}:                          {format: "docker"},
		{"demo-raw", "docs/readme.txt"}:                                      {"raw", "/docs", "docs/readme.txt", "", "", true, false},
		{"demo-hinted", "com/example/hinted/1.0/hinted-1.0.pom"}:             {"maven2", "com.example", "hinted", "1.0", "pom", true, false},
		{"demo-plain", "notes/plain.txt"}:                                    {format: "unknown"},
	} {
		a := assetOf(t, db, k)
		got := coordinates{a.format, a.namespace.String, a.name.String, a.version.String, a.extension.String, a.component.Valid,
			a.prerelease.Bool}
		if got != want {
			t.Fatalf("%v: %+v, want %+v", k, got, want)
		}
	}
	jar := assetOf(t, db, key{"demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar"})
	pom := assetOf(t, db, key{"demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.pom"})
	if jar.component != pom.component {
		t.Fatalf("jar and pom of one version have components %v and %v", jar.component, pom.component)
	}
	if n := count(t, db, "SELECT count(*) FROM components c WHERE NOT EXISTS (SELECT 1 FROM assets a WHERE a.component = c.id)"); n != 0 {
		t.Fatalf("%d components without an asset", n)
	}
	leftovers, err := filepath.Glob(filepath.Join(f.dir, catalog.FileName+".fill-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("staging files left: %v %v", leftovers, err)
	}
}

// A second scan of the same stores changes no row; a changed store changes
// its rows, and the winners of the paths it shares follow.
func TestRescanFollowsTheStore(t *testing.T) {
	f := newFixture(t)
	f.scan(f.north)
	f.scan(f.south)
	db := reader(t, f.dir)
	before := tableText(t, db)
	f.scan(f.north)
	f.scan(f.south)
	if after := tableText(t, db); after != before {
		t.Fatalf("a scan of unchanged stores changed the catalog\nbefore\n%s\nafter\n%s", before, after)
	}

	// south loses its readme: the north blob wins again
	readme := southSpecs[0]
	for _, location := range []string{testkit.BytesLocation(readme), testkit.PropertiesLocation(readme)} {
		if err := os.Remove(filepath.Join(f.south.Root(), filepath.FromSlash(location))); err != nil {
			t.Fatal(err)
		}
	}
	f.scan(f.south)
	f.equalsIndex(db, keysOf(northSpecs, southSpecs))
	if a := assetOf(t, db, key{"demo-raw", "docs/readme.txt"}); a == nil || a.store != "north" {
		t.Fatalf("readme after its south blob went away: %+v", a)
	}

	// north loses its only blob of demo-plain: the asset and its repository go
	plain := northSpecs[16]
	for _, location := range []string{testkit.BytesLocation(plain), testkit.PropertiesLocation(plain)} {
		if err := os.Remove(filepath.Join(f.north.Root(), filepath.FromSlash(location))); err != nil {
			t.Fatal(err)
		}
	}
	f.scan(f.north)
	f.equalsIndex(db, keysOf(northSpecs, southSpecs))
	if a := assetOf(t, db, key{"demo-plain", "notes/plain.txt"}); a != nil {
		t.Fatalf("an asset of a removed blob: %+v", a)
	}
}

// tableText is every row of the tables a scan fills, sorted.
func tableText(t *testing.T, db *sql.DB) string {
	t.Helper()
	var lines []string
	for _, query := range []string{
		"SELECT 'blob', store, repo, path, size, coalesce(sha1, '-'), coalesce(content_type, '-'), created_ms," +
			" coalesce(uploader, '-'), coalesce(uploader_ip, '-') FROM blobs",
		"SELECT 'asset', a.repo, a.path, a.store, a.format, coalesce(c.namespace, '-'), coalesce(c.name, '-')," +
			" coalesce(c.version, '-'), coalesce(a.extension, '-'), coalesce(a.classifier, '-') FROM assets a" +
			" LEFT JOIN components c ON c.id = a.component",
		"SELECT 'component', id, repo, format, namespace, name, version, prerelease, '', '' FROM components",
		"SELECT 'scan', store, blobs, bytes, '', '', '', '', '', '' FROM scans",
	} {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, 10)
			pointers := make([]any, 10)
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, fmt.Sprint(values...))
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// A store the index no longer holds loses its rows at the next scan, and its
// paths fall to the stores that remain.
func TestStoreLeftTheIndex(t *testing.T) {
	f := newFixture(t)
	f.scan(f.north)
	f.scan(f.south)
	if err := f.ix.RemoveStore("south"); err != nil {
		t.Fatal(err)
	}
	f.scan(f.north)
	db := reader(t, f.dir)
	f.equalsIndex(db, keysOf(northSpecs, southSpecs))
	if n := count(t, db, "SELECT count(*) FROM blobs WHERE store = 'south'"); n != 0 {
		t.Fatalf("%d blob rows of the removed store", n)
	}
	if a := assetOf(t, db, key{"demo-raw", "docs/readme.txt"}); a == nil || a.store != "north" {
		t.Fatalf("readme after its store left: %+v", a)
	}
}

// failingStore yields its entries and then fails, as a store whose disk goes
// away during the walk.
type failingStore struct {
	stores.Store
	err error
}

func (s failingStore) Entries(counts *stores.ScanCounts, yield func(stores.Entry) error) error {
	if err := s.Store.Entries(counts, yield); err != nil {
		return err
	}
	return s.err
}

// A walk that fails leaves the index and the catalog as they were and no
// staging file behind.
func TestFailedWalkLeavesTheCatalog(t *testing.T) {
	f := newFixture(t)
	f.scan(f.north)
	db := reader(t, f.dir)
	before := tableText(t, db)
	broken := errors.New("the disk went away")
	if _, err := scan.Run(failingStore{Store: f.south, err: broken}, f.ix, nil); !errors.Is(err, broken) {
		t.Fatalf("scan of a failing store: %v", err)
	}
	if after := tableText(t, db); after != before {
		t.Fatalf("a failed walk changed the catalog\n%s", after)
	}
	leftovers, err := filepath.Glob(filepath.Join(f.dir, catalog.FileName+".fill-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("staging files left: %v %v", leftovers, err)
	}
}

// A catalog the binary cannot use fails no scan: the index takes the rows,
// the catalog file stays as it is, and a warning names the file.
func TestUnusableCatalogFailsNoScan(t *testing.T) {
	f := newFixture(t)
	path := catalog.Path(f.dir)
	db, err := sql.Open("sqlite", fileDSN(t, path, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{migrateMeta, "INSERT INTO meta VALUES ('schema', 99)", "CREATE TABLE later (id INTEGER)"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(previous)
	result := f.scan(f.north)
	if result.Live != len(northSpecs) {
		t.Fatalf("scan counts %d live blobs, want %d", result.Live, len(northSpecs))
	}
	if a, err := f.ix.Lookup("demo-raw", "docs/readme.txt"); err != nil || a == nil {
		t.Fatalf("the index lost the scan: %v %v", a, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("the catalog file changed: %v", err)
	}
	if text := logged.String(); !strings.Contains(text, "level=WARN") || !strings.Contains(text, "has schema 99") {
		t.Fatalf("no warning about the catalog: %q", text)
	}
}

const migrateMeta = "CREATE TABLE meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL) WITHOUT ROWID"

// A staging file of a fill that ended more than a day ago is removed by the
// next fill; one of a fill that may still run stays.
func TestOldStagingFilesAreRemoved(t *testing.T) {
	f := newFixture(t)
	old := filepath.Join(f.dir, catalog.FileName+".fill-old")
	fresh := filepath.Join(f.dir, catalog.FileName+".fill-fresh")
	for _, name := range []string{old, fresh} {
		if err := os.WriteFile(name, []byte("left behind"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	day := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(old, day, day); err != nil {
		t.Fatal(err)
	}
	f.scan(f.north)
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old staging file is still there: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("the fresh staging file is gone: %v", err)
	}
}

// 16 readers in read transactions during the fill of a store see the old
// rows or the new rows of that store, never a mix.
func TestReadersSeeOldOrNewRows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	c, err := catalog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	formats := func(string) string { return "raw" }
	fill := func(prefix string, created int64, n int) {
		t.Helper()
		f, err := c.Begin("demo-store")
		if err != nil {
			t.Fatal(err)
		}
		sha1 := "86f7e437faa5a7fce15d1ddcb9eaeaea377667b8"
		for i := range n {
			e := stores.Entry{Repo: "demo-raw", Path: fmt.Sprintf("%s/%06d.txt", prefix, i), Size: 1, SHA1: &sha1, CreatedMS: created,
				Location: fmt.Sprintf("content/%s/%06d.bytes", prefix, i)}
			if err := f.Add(e); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.Commit(formats, []string{"demo-store"}); err != nil {
			t.Fatal(err)
		}
	}
	fill("old", 1, 500)

	type seen struct{ blobs, assets, components, low, high int64 }
	// a raw path is a component of its own
	valid := map[seen]string{{500, 500, 500, 1, 1}: "old", {2_000, 2_000, 2_000, 2, 2}: "new"}
	db := reader(t, dir)
	db.SetMaxOpenConns(16)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[string]int{}
	var mixed []seen
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tx, err := db.Begin()
				if err != nil {
					t.Error(err)
					return
				}
				var s seen
				err = tx.QueryRow("SELECT count(*), min(created_ms), max(created_ms) FROM blobs WHERE store = 'demo-store'").Scan(&s.blobs, &s.low, &s.high)
				if err == nil {
					err = tx.QueryRow("SELECT count(*) FROM assets WHERE store = 'demo-store'").Scan(&s.assets)
				}
				if err == nil {
					err = tx.QueryRow("SELECT count(*) FROM components").Scan(&s.components)
				}
				_ = tx.Rollback()
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if name, ok := valid[s]; ok {
					counts[name]++
				} else {
					mixed = append(mixed, s)
				}
				mu.Unlock()
				time.Sleep(time.Millisecond)
			}
		}()
	}
	// on a busy machine the readers start late: wait for an observation of
	// the old rows before the fill and of the new rows after it, 10 s at most
	waitFor := func(name string) {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			mu.Lock()
			n := counts[name]
			mu.Unlock()
			if n > 0 {
				return
			}
		}
	}
	waitFor("old")
	fill("new", 2, 2_000)
	waitFor("new")
	close(stop)
	wg.Wait()
	if len(mixed) > 0 || counts["old"] == 0 || counts["new"] == 0 {
		t.Fatalf("readers saw old %d times, new %d times, a mix %d times (first %+v)", counts["old"], counts["new"], len(mixed), mixed)
	}
	t.Logf("old %d, new %d", counts["old"], counts["new"])
}
