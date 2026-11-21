package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// The fixture testdata/v3.sqlite is a catalog file of schema 3 with invented
// rows, made like v2.sqlite: its generator holds step 3 as a text of its
// own. The flag -write-fixture writes it again:
//
//	tools/go.sh run 'go test ./internal/catalog -run "^TestFixtureV3IsItsGeneratorOutput$" -write-fixture'
const fixtureV3 = "testdata/v3.sqlite"

// schemaV3 is step 3 as phase 5 wrote it.
var schemaV3 = []string{
	"CREATE TABLE IF NOT EXISTS proxy_state (repo TEXT NOT NULL, path TEXT NOT NULL, checked_ms INTEGER, etag TEXT," +
		" last_modified TEXT, not_found_until_ms INTEGER, PRIMARY KEY (repo, path)) WITHOUT ROWID",
}

// rowsV3 are the rows of v2 and two proxy rows: a checked path with its
// validators and the reset row of a repository.
var rowsV3 = append(slices.Clip(rowsV2), []struct {
	table  string
	values []any
}{
	{"proxy_state", []any{"demo-proxy", "com/example/demo-lib/1.2.0/demo-lib-1.2.0.pom", int64(1_700_000_000_300),
		`"demo-etag"`, "Tue, 14 Nov 2023 22:13:20 GMT", nil}},
	{"proxy_state", []any{"demo-proxy", "", int64(1_700_000_000_400), nil, nil, nil}},
}...)

// generateV3 writes the fixture of schema 3 at path, a new file.
func generateV3(path string) error {
	return generateFixture(path, slices.Concat(schemaV1[1:], schemaV2, schemaV3), 3, rowsV3)
}

// The committed v3.sqlite is what its generator writes.
func TestFixtureV3IsItsGeneratorOutput(t *testing.T) {
	generated := filepath.Join(t.TempDir(), "v3.sqlite")
	if err := generateV3(generated); err != nil {
		t.Fatal(err)
	}
	if *writeFixture {
		data, err := os.ReadFile(generated)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixtureV3, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := copied(t, fixtureV3, filepath.Join(t.TempDir(), "fixture.sqlite"))
	if got, want := dump(t, fixture, true), dump(t, generated, true); got != want {
		t.Fatalf("testdata/v3.sqlite differs from its generator\nfixture\n%s\ngenerator\n%s", got, want)
	}
}

// Steps 1 to 3 of migrations.go make the schema of the fixture of version 3.
func TestStep3MakesTheSchemaOfTheFixture(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "data"))
	c, err := openWith(path, steps[:3])
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	fixture := copied(t, fixtureV3, filepath.Join(t.TempDir(), "fixture.sqlite"))
	if got, want := dump(t, path, false), dump(t, fixture, false); got != want {
		t.Fatalf("steps 1 to 3 differ from the fixture\nsteps\n%s\nfixture\n%s", got, want)
	}
}

// Version 4 adds downloads and request_days (decisions L11 and L12 of the
// phase 7 plan). Neither table has a column for a user or an address.
func TestVersion4HasTheUsageTables(t *testing.T) {
	if SchemaVersion < 4 {
		t.Fatalf("SchemaVersion %d, want 4 or later", SchemaVersion)
	}
	dir := filepath.Join(t.TempDir(), "data")
	c := mustOpen(t, dir)
	defer func() { _ = c.Close() }()
	want := map[string][]string{
		"downloads":    {"repo", "day", "count", "bytes"},
		"request_days": {"day", "requests", "bytes_sent"},
	}
	for table, columns := range want {
		rows, err := c.readers.Query("SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, columns) {
			t.Fatalf("table %s has columns %v, want %v", table, got, columns)
		}
		for _, name := range got {
			for _, personal := range []string{"user", "address", "client", "ip", "uploader", "subject"} {
				if strings.Contains(name, personal) {
					t.Fatalf("table %s has the column %s", table, name)
				}
			}
		}
	}
}

// A binary of version 3 refuses a file of version 4, names both versions,
// and leaves the file as it was.
func TestVersion3BinaryRefusesAVersion4File(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := Path(dir)
	c, err := openWith(path, steps[:4])
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	before := dump(t, path, true)
	old, err := openWith(path, steps[:3])
	if err == nil {
		_ = old.Close()
		t.Fatal("a binary of version 3 opened a file of version 4")
	}
	var newer *migrate.NewerError
	if !errors.As(err, &newer) || newer.Found != 4 || newer.Knows != 3 {
		t.Fatalf("error %v (%T), want a NewerError with 4 and 3", err, err)
	}
	if want := path + " has schema 4, this binary knows schema 3"; err.Error() != want {
		t.Fatalf("error %q, want %q", err, want)
	}
	if after := dump(t, path, true); after != before {
		t.Fatalf("the refused file changed\nbefore\n%s\nafter\n%s", before, after)
	}
	missing(t, path+".v"+strconv.Itoa(3))
}
