package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The fixture testdata/v2.sqlite is a catalog file of schema 2 with invented
// rows, in the style of v1.sqlite: its generator below holds step 2 as a text
// of its own, so a change of step 2 in migrations.go shows as a difference
// from the fixture. The flag -write-fixture of catalog_test.go writes it
// again:
//
//	tools/go.sh run 'go test ./internal/catalog -run "^TestFixtureV2IsItsGeneratorOutput$" -write-fixture'
const fixtureV2 = "testdata/v2.sqlite"

// schemaV2 is step 2 as leg F1a wrote it.
var schemaV2 = []string{
	"CREATE TABLE IF NOT EXISTS imported_assets (id INTEGER PRIMARY KEY, repo TEXT NOT NULL, path TEXT NOT NULL," +
		" blob_ref TEXT NOT NULL, old_id TEXT, last_updated_ms INTEGER, last_downloaded_ms INTEGER," +
		" checksums TEXT NOT NULL, kind TEXT, namespace TEXT, name TEXT, version TEXT, attributes TEXT," +
		" component_attributes TEXT)",
	"CREATE UNIQUE INDEX IF NOT EXISTS imported_assets_path ON imported_assets (repo, path) WHERE path <> ''",
	"CREATE INDEX IF NOT EXISTS imported_assets_blob ON imported_assets (blob_ref)",
}

// rowsV2 are the rows of v1 and two imported rows: an asset with its path
// and a blob without an asset, which has the empty path.
var rowsV2 = append(slices.Clip(rowsV1), []struct {
	table  string
	values []any
}{
	{"imported_assets", []any{1, "demo-maven", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar", "demo-store@0e9a", "old-asset-1",
		int64(1_700_000_000_100), nil, `{"sha1":"` + jarSHA1 + `"}`, "maven", "com.example", "demo-lib", "1.2.0", "{}", "{}"}},
	{"imported_assets", []any{2, "demo-raw", "", "demo-store@0e9b", nil, nil, int64(1_700_000_000_200), "{}", nil, nil, nil,
		nil, nil, nil}},
}...)

// generateFixture writes a catalog file of the given schema version at path,
// a new file: the statements after the meta table, then the rows.
func generateFixture(path string, statements []string, version int, rows []struct {
	table  string
	values []any
}) error {
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
		for _, statement := range append(append([]string{}, schemaV1[:1]...), statements...) {
			if _, err = tx.Exec(statement); err != nil {
				break
			}
		}
	}
	if err == nil {
		_, err = tx.Exec("INSERT INTO meta VALUES ('schema', ?)", version)
	}
	for _, row := range rows {
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

// generateV2 writes the fixture of schema 2 at path, a new file.
func generateV2(path string) error {
	return generateFixture(path, slices.Concat(schemaV1[1:], schemaV2), 2, rowsV2)
}

// The committed v2.sqlite is what its generator writes.
func TestFixtureV2IsItsGeneratorOutput(t *testing.T) {
	generated := filepath.Join(t.TempDir(), "v2.sqlite")
	if err := generateV2(generated); err != nil {
		t.Fatal(err)
	}
	if *writeFixture {
		data, err := os.ReadFile(generated)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixtureV2, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := copied(t, fixtureV2, filepath.Join(t.TempDir(), "fixture.sqlite"))
	if got, want := dump(t, fixture, true), dump(t, generated, true); got != want {
		t.Fatalf("testdata/v2.sqlite differs from its generator\nfixture\n%s\ngenerator\n%s", got, want)
	}
}

// Steps 1 and 2 of migrations.go make the schema of the fixture of version 2,
// so step 2 has not changed since the fixture was written.
func TestStep2MakesTheSchemaOfTheFixture(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "data"))
	c, err := openWith(path, steps[:2])
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	fixture := copied(t, fixtureV2, filepath.Join(t.TempDir(), "fixture.sqlite"))
	if got, want := dump(t, path, false), dump(t, fixture, false); got != want {
		t.Fatalf("steps 1 and 2 differ from the fixture\nsteps\n%s\nfixture\n%s", got, want)
	}
}

// The migration test of the overview (docs/plans/phases.md, section State):
// a fixture file of each earlier version, migrated, has the schema of a
// fresh file, keeps its rows, carries the schema row of SchemaVersion, and
// its proxy_state is empty.
func TestFixturesOfEachEarlierVersionEqualAFreshFile(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "fresh")
	f := mustOpen(t, fresh)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fixtures := map[int]string{1: fixtureV1, 2: fixtureV2, 3: fixtureV3, 4: fixtureV4, 5: fixtureV5}
	if len(fixtures) != SchemaVersion-1 {
		t.Fatalf("%d fixtures for the %d versions before %d", len(fixtures), SchemaVersion-1, SchemaVersion)
	}
	for version, fixture := range fixtures {
		dir := filepath.Join(t.TempDir(), "data")
		path := copied(t, fixture, Path(dir))
		before := dump(t, path, true)
		if !strings.Contains(before, `meta: string "schema", int64 "`+strconv.Itoa(version)+`"`) {
			t.Fatalf("%s has no schema row %d", fixture, version)
		}
		c := mustOpen(t, dir)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if got, want := dump(t, path, false), dump(t, Path(fresh), false); got != want {
			t.Fatalf("%s, migrated, differs from a fresh file\nmigrated\n%s\nfresh\n%s", fixture, got, want)
		}
		after := dump(t, path, true)
		for _, line := range strings.Split(before, "\n") {
			if strings.HasPrefix(line, "table ") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "meta: ") {
				continue
			}
			if !strings.Contains(after, line) {
				t.Fatalf("%s: row lost by the migration: %s", fixture, line)
			}
		}
		if !strings.Contains(after, `meta: string "schema", int64 "`+strconv.Itoa(SchemaVersion)+`"`) {
			t.Fatalf("%s, migrated, has no schema row %d:\n%s", fixture, SchemaVersion, after)
		}
		for _, line := range strings.Split(after, "\n") {
			if version < 3 && strings.HasPrefix(line, "proxy_state: ") {
				t.Fatalf("%s: proxy_state holds a row after the migration: %s", fixture, line)
			}
		}
	}
}
