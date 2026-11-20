package catalog

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// The fixture testdata/v1.sqlite is a catalog file of schema 1 with invented
// rows. Its generator below holds schema 1 as a text of its own, so a change
// of step 1 in migrations.go shows as a difference from the fixture. The file
// is written again with
//
//	tools/go.sh run 'go test ./internal/catalog -run "^TestFixtureV1IsItsGeneratorOutput$" -write-fixture'
var writeFixture = flag.Bool("write-fixture", false, "write testdata/v1.sqlite from its generator")

const fixtureV1 = "testdata/v1.sqlite"

// schemaV1 is schema 1 as this leg first wrote it, the meta table of the
// runner first.
var schemaV1 = []string{
	migrate.Meta,
	"CREATE TABLE IF NOT EXISTS scans (store TEXT PRIMARY KEY, blobs INTEGER NOT NULL, bytes INTEGER NOT NULL," +
		" scanned_ms INTEGER) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS blobs (store TEXT NOT NULL, repo TEXT NOT NULL, path TEXT NOT NULL," +
		" size INTEGER NOT NULL, sha1 TEXT, content_type TEXT, created_ms INTEGER NOT NULL, uploader TEXT," +
		" uploader_ip TEXT, md5 TEXT, sha256 TEXT, sha512 TEXT, PRIMARY KEY (store, repo, path)) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS components (id INTEGER PRIMARY KEY, repo TEXT NOT NULL, format TEXT NOT NULL," +
		" namespace TEXT NOT NULL, name TEXT NOT NULL, version TEXT NOT NULL, prerelease INTEGER NOT NULL," +
		" UNIQUE (repo, format, namespace, name, version))",
	"CREATE TABLE IF NOT EXISTS assets (repo TEXT NOT NULL, path TEXT NOT NULL, store TEXT NOT NULL," +
		" format TEXT NOT NULL, component INTEGER REFERENCES components (id), extension TEXT, classifier TEXT," +
		" last_modified_ms INTEGER, last_downloaded_ms INTEGER, PRIMARY KEY (repo, path)," +
		" FOREIGN KEY (store, repo, path) REFERENCES blobs (store, repo, path)) WITHOUT ROWID",
	"CREATE INDEX IF NOT EXISTS assets_component ON assets (component) WHERE component IS NOT NULL",
}

const (
	jarSHA1 = "5d41402abc4b2a76b9719d911017c592ae7f6a3c"
	pomSHA1 = "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"
)

// rowsV1 are the invented rows of the fixture, in an order the foreign keys
// accept.
var rowsV1 = []struct {
	table  string
	values []any
}{
	{"scans", []any{"demo-store", 3, 30, int64(1_700_000_000_000)}},
	{"blobs", []any{"demo-store", "demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar", 10, jarSHA1,
		"application/java-archive", int64(1_700_000_000_000), "demo-user", "192.0.2.10", nil, nil, nil}},
	{"blobs", []any{"demo-store", "demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.pom", 12, pomSHA1,
		"application/xml", int64(1_700_000_000_001), "demo-user", "192.0.2.10", nil, nil, nil}},
	{"blobs", []any{"demo-store", "demo-raw", "docs/readme.txt", 8, nil, "text/plain", int64(1_700_000_000_002), nil,
		nil, nil, nil, nil}},
	{"components", []any{1, "demo-maven", "maven2", "com.example", "demo-lib", "1.2.0", 0}},
	{"components", []any{2, "demo-raw", "raw", "/docs", "docs/readme.txt", "", 0}},
	{"assets", []any{"demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar", "demo-store", "maven2", 1, "jar",
		nil, nil, nil}},
	{"assets", []any{"demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.pom", "demo-store", "maven2", 1, "pom",
		nil, nil, nil}},
	{"assets", []any{"demo-raw", "docs/readme.txt", "demo-store", "raw", 2, nil, nil, nil, nil}},
}

// generateV1 writes the fixture of schema 1 at path, a new file.
func generateV1(path string) error {
	name, err := dsn(path, url.Values{"_pragma": {"foreign_keys(1)"}})
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", name)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err == nil {
		for _, statement := range schemaV1 {
			if _, err = tx.Exec(statement); err != nil {
				break
			}
		}
	}
	if err == nil {
		_, err = tx.Exec("INSERT INTO meta VALUES ('schema', 1)")
	}
	for _, row := range rowsV1 {
		if err != nil {
			break
		}
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(row.values)), ", ")
		if _, err = tx.Exec("INSERT INTO "+row.table+" VALUES ("+marks+")", row.values...); err != nil {
			err = fmt.Errorf("%s %v: %w", row.table, row.values[1], err)
		}
	}
	if err == nil {
		err = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback() // the step error is returned
	}
	return errors.Join(err, db.Close())
}

// copied copies a file to a new file of mode 0600. A fixture is never
// opened where it lies, so that no WAL file appears beside it.
func copied(t *testing.T, from, to string) string {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return to
}

// dump is the text of a SQLite file opened read only: every object of the
// schema with its SQL and, with rows, every row of every table, sorted, with
// the type of each value.
func dump(t *testing.T, path string, rows bool) string {
	t.Helper()
	name, err := dsn(path, url.Values{"mode": {"ro"}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var lines, tables []string
	objects, err := db.Query("SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master" +
		" WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name")
	if err != nil {
		t.Fatal(err)
	}
	for objects.Next() {
		var kind, name, table, text string
		if err := objects.Scan(&kind, &name, &table, &text); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%s %s on %s: %s", kind, name, table, text))
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	if err := objects.Close(); err != nil {
		t.Fatal(err)
	}
	if !rows {
		return strings.Join(lines, "\n")
	}
	for _, table := range tables {
		list, err := db.Query("SELECT * FROM " + table) // #nosec G202 -- a table name read from sqlite_master of a test file.
		if err != nil {
			t.Fatal(err)
		}
		columns, err := list.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for list.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := list.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			cells := make([]string, len(values))
			for i, v := range values {
				cells[i] = fmt.Sprintf("%T %q", v, fmt.Sprint(v))
			}
			found = append(found, table+": "+strings.Join(cells, ", "))
		}
		if err := list.Close(); err != nil {
			t.Fatal(err)
		}
		slices.Sort(found)
		lines = append(lines, found...)
	}
	return strings.Join(lines, "\n")
}

func scalar[T any](t *testing.T, db *sql.DB, query string) T {
	t.Helper()
	var v T
	if err := db.QueryRow(query).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

func mustOpen(t *testing.T, dataDir string) *Catalog {
	t.Helper()
	c, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func probeStep(version int) migrate.Step {
	return migrate.Step{Version: version, Apply: func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE probe (id INTEGER PRIMARY KEY)")
		return err
	}}
}

func failingStep(version int) migrate.Step {
	return migrate.Step{Version: version, Apply: func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE TABLE probe (id INTEGER PRIMARY KEY)"); err != nil {
			return err
		}
		return errors.New("probe step fails")
	}}
}

func missing(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s is still there: %v", name, err)
		}
	}
}

// A fresh file has schema 4 with the tables of every step and no search
// table, mode 0600, WAL mode, foreign keys on and one writer connection.
func TestFreshFileHasTheLastSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	c := mustOpen(t, dir)
	if c.File() != Path(dir) {
		t.Fatalf("file %s, want %s", c.File(), Path(dir))
	}
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode %o, want 600: the file holds the names and addresses of uploaders", perm)
	}
	if got := scalar[int](t, c.writer, "SELECT value FROM meta WHERE key = 'schema'"); got != SchemaVersion || SchemaVersion != 4 {
		t.Fatalf("schema %d, SchemaVersion %d, want 4", got, SchemaVersion)
	}
	rows, err := c.readers.Query("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"assets", "blobs", "components", "downloads", "imported_assets", "meta", "proxy_state", "request_days", "scans"}; !slices.Equal(tables, want) {
		t.Fatalf("tables %v, want %v", tables, want)
	}
	if mode := scalar[string](t, c.writer, "PRAGMA journal_mode"); mode != "wal" {
		t.Fatalf("journal mode %s, want wal", mode)
	}
	if on := scalar[int](t, c.writer, "PRAGMA foreign_keys"); on != 1 {
		t.Fatal("foreign keys are off on the writer")
	}
	if n := c.writer.Stats().MaxOpenConnections; n != 1 {
		t.Fatalf("the writer pool allows %d connections, want 1", n)
	}
	if _, err := c.readers.Exec("DELETE FROM scans"); err == nil {
		t.Fatal("a reader connection wrote the file")
	}
	// a second open of the file takes no step and changes nothing
	before := dump(t, Path(dir), true)
	again := mustOpen(t, dir)
	_ = again.Close()
	if after := dump(t, Path(dir), true); after != before {
		t.Fatalf("a second open changed the file\nbefore\n%s\nafter\n%s", before, after)
	}
}

// A file of a newer schema is refused with both versions named and stays as
// it is, without a copy.
func TestNewerFileIsRefusedAndKept(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := Path(dir)
	c, err := openWith(path, append(slices.Clip(steps), probeStep(SchemaVersion+1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	before := dump(t, path, true)
	_, err = Open(dir)
	want := path + " has schema " + strconv.Itoa(SchemaVersion+1) + ", this binary knows schema " + strconv.Itoa(SchemaVersion)
	if err == nil || err.Error() != want {
		t.Fatalf("Open of a newer file: %v, want %q", err, want)
	}
	if after := dump(t, path, true); after != before {
		t.Fatalf("the refusal changed the file\n%s", after)
	}
	missing(t, path+".v"+strconv.Itoa(SchemaVersion))
}

// A failing step removes a file this open created, with its WAL files.
func TestFailingMigrationRemovesTheFile(t *testing.T) {
	t.Run("a new file", func(t *testing.T) {
		path := Path(filepath.Join(t.TempDir(), "data"))
		_, err := openWith(path, []migrate.Step{{Version: 1, Apply: schema1}, failingStep(2)})
		if err == nil {
			t.Fatal("a failing step gave no error")
		}
		missing(t, path, path+"-wal", path+"-shm")
	})
}

// A failing step keeps a file that existed. After a failed first step the
// file is as it was and no copy is left; after a failed later step the file
// is at the version of the last step that passed and the copy of the version
// before stays beside it.
func TestFailingMigrationKeepsAnExistingFile(t *testing.T) {
	existing := func(t *testing.T) (string, string) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "data")
		c := mustOpen(t, dir)
		if _, err := c.writer.Exec("INSERT INTO scans VALUES ('demo-store', 1, 2, NULL)"); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		return dir, dump(t, Path(dir), true)
	}
	fails := func(version int) migrate.Step {
		return migrate.Step{Version: version, Apply: func(*sql.Tx) error { return errors.New("probe step fails") }}
	}
	t.Run("the first step fails", func(t *testing.T) {
		dir, before := existing(t)
		path := Path(dir)
		_, err := openWith(path, append(slices.Clip(steps), failingStep(SchemaVersion+1)))
		if err == nil || !strings.Contains(err.Error(), "probe step fails") {
			t.Fatalf("open with a failing step: %v", err)
		}
		if after := dump(t, path, true); after != before {
			t.Fatalf("the failed step changed the file\nafter\n%s\nbefore\n%s", after, before)
		}
		missing(t, path+".v"+strconv.Itoa(SchemaVersion))
		c := mustOpen(t, dir)
		if n := scalar[int](t, c.writer, "SELECT count(*) FROM scans WHERE store = 'demo-store'"); n != 1 {
			t.Fatalf("the file holds %d rows of scans after the next open, want 1", n)
		}
	})
	t.Run("a later step fails", func(t *testing.T) {
		dir, before := existing(t)
		path := Path(dir)
		_, err := openWith(path, append(slices.Clip(steps), probeStep(SchemaVersion+1), fails(SchemaVersion+2)))
		if err == nil || !strings.Contains(err.Error(), "probe step fails") {
			t.Fatalf("open with a failing second step: %v", err)
		}
		got, err := migrate.FileVersion(path)
		if err != nil || got != SchemaVersion+1 {
			t.Fatalf("the file is at schema %d (%v), want %d", got, err, SchemaVersion+1)
		}
		if n := strings.Count(dump(t, path, true), "\nscans: string"); n != 1 {
			t.Fatalf("the file holds %d rows of scans, want 1", n)
		}
		if saved := dump(t, path+".v"+strconv.Itoa(SchemaVersion), true); saved != before {
			t.Fatalf("the copy differs from the file before\ncopy\n%s\nbefore\n%s", saved, before)
		}
	})
}

// A file that is no catalog file is refused before any step and kept.
func TestForeignFileIsRefusedAndKept(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := Path(dir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	name, err := dsn(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE other (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := dump(t, path, true)
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "no meta table, so it is no catalog file") {
		t.Fatalf("Open of a foreign file: %v", err)
	}
	if after := dump(t, path, true); after != before {
		t.Fatalf("the refusal changed the file\n%s", after)
	}
}

// The fixture of schema 1, migrated, has the schema of a fresh file and keeps
// its rows; imported_assets is empty.
func TestFixtureOfVersion1EqualsAFreshFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := copied(t, fixtureV1, Path(dir))
	rowsBefore := dump(t, path, true)
	c := mustOpen(t, dir)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	f := mustOpen(t, fresh)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := dump(t, path, false), dump(t, Path(fresh), false); got != want {
		t.Fatalf("the migrated fixture differs from a fresh file\nmigrated\n%s\nfresh\n%s", got, want)
	}
	rowsAfter := dump(t, path, true)
	for _, line := range strings.Split(rowsBefore, "\n") {
		if strings.HasPrefix(line, "table ") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "meta: ") {
			continue
		}
		if !strings.Contains(rowsAfter, line) {
			t.Fatalf("row lost by the migration: %s", line)
		}
	}
	if !strings.Contains(rowsAfter, `meta: string "schema", int64 "`+strconv.Itoa(SchemaVersion)+`"`) {
		t.Fatalf("the migrated fixture has no schema row %d:\n%s", SchemaVersion, rowsAfter)
	}
	for _, line := range strings.Split(rowsAfter, "\n") {
		if strings.HasPrefix(line, "imported_assets: ") {
			t.Fatalf("imported_assets holds a row after the migration: %s", line)
		}
	}
}

// Step 1 of migrations.go makes the schema of the fixture, so step 1 has not
// changed since the fixture was written.
func TestStep1MakesTheSchemaOfTheFixture(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "data"))
	c, err := openWith(path, steps[:1])
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	fixture := copied(t, fixtureV1, filepath.Join(t.TempDir(), "fixture.sqlite"))
	if got, want := dump(t, path, false), dump(t, fixture, false); got != want {
		t.Fatalf("step 1 differs from the fixture\nstep 1\n%s\nfixture\n%s", got, want)
	}
}

// The committed fixture is what its generator writes.
func TestFixtureV1IsItsGeneratorOutput(t *testing.T) {
	generated := filepath.Join(t.TempDir(), "v1.sqlite")
	if err := generateV1(generated); err != nil {
		t.Fatal(err)
	}
	if *writeFixture {
		data, err := os.ReadFile(generated)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixtureV1, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := copied(t, fixtureV1, filepath.Join(t.TempDir(), "fixture.sqlite"))
	if got, want := dump(t, fixture, true), dump(t, generated, true); got != want {
		t.Fatalf("testdata/v1.sqlite differs from its generator\nfixture\n%s\ngenerator\n%s", got, want)
	}
}

// The step list ends at SchemaVersion.
func TestStepsEndAtSchemaVersion(t *testing.T) {
	if len(steps) != SchemaVersion || steps[len(steps)-1].Version != SchemaVersion {
		t.Fatalf("%d steps, the last of version %d; SchemaVersion is %d", len(steps), steps[len(steps)-1].Version, SchemaVersion)
	}
}
