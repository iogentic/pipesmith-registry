package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// The fixture testdata/v4.sqlite is a catalog file of schema 4 with invented
// rows, made like v3.sqlite: its generator holds step 4 as a text of its
// own. The flag -write-fixture writes it again:
//
//	tools/go.sh run 'go test ./internal/catalog -run "^TestFixtureV4IsItsGeneratorOutput$" -write-fixture'
const fixtureV4 = "testdata/v4.sqlite"

// schemaV4 is step 4 as phase 7 wrote it.
var schemaV4 = []string{
	"CREATE TABLE IF NOT EXISTS downloads (repo TEXT NOT NULL, day TEXT NOT NULL, count INTEGER NOT NULL," +
		" bytes INTEGER NOT NULL, PRIMARY KEY (repo, day)) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS request_days (day TEXT PRIMARY KEY, requests INTEGER NOT NULL," +
		" bytes_sent INTEGER NOT NULL) WITHOUT ROWID",
}

// rowsV4 are the rows of v3 and one row of each usage table.
var rowsV4 = append(slices.Clip(rowsV3), []struct {
	table  string
	values []any
}{
	{"downloads", []any{"demo-raw", "2023-11-14", 3, 24}},
	{"request_days", []any{"2023-11-14", 5, 40}},
}...)

// generateV4 writes the fixture of schema 4 at path, a new file.
func generateV4(path string) error {
	return generateFixture(path, slices.Concat(schemaV1[1:], schemaV2, schemaV3, schemaV4), 4, rowsV4)
}

// The committed v4.sqlite is what its generator writes.
func TestFixtureV4IsItsGeneratorOutput(t *testing.T) {
	generated := filepath.Join(t.TempDir(), "v4.sqlite")
	if err := generateV4(generated); err != nil {
		t.Fatal(err)
	}
	if *writeFixture {
		data, err := os.ReadFile(generated)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixtureV4, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := copied(t, fixtureV4, filepath.Join(t.TempDir(), "fixture.sqlite"))
	if got, want := dump(t, fixture, true), dump(t, generated, true); got != want {
		t.Fatalf("testdata/v4.sqlite differs from its generator\nfixture\n%s\ngenerator\n%s", got, want)
	}
}

// Steps 1 to 4 of migrations.go make the schema of the fixture of version 4.
func TestStep4MakesTheSchemaOfTheFixture(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "data"))
	c, err := openWith(path, steps[:4])
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	fixture := copied(t, fixtureV4, filepath.Join(t.TempDir(), "fixture.sqlite"))
	if got, want := dump(t, path, false), dump(t, fixture, false); got != want {
		t.Fatalf("steps 1 to 4 differ from the fixture\nsteps\n%s\nfixture\n%s", got, want)
	}
}

// The fixture of version 4, migrated, keeps its rows and takes the time of
// its migration as its build time (ruling CL3 as the review of leg
// 6-cleanup changed it: a usage policy selects none of its assets before
// the criterion age passed).
func TestMigratedFixtureV4TakesItsMigrationTime(t *testing.T) {
	before := buildClock
	t.Cleanup(func() { buildClock = before })
	buildClock = func() time.Time { return dlStart }
	dir := filepath.Join(t.TempDir(), "data")
	copied(t, fixtureV4, Path(dir))
	c := mustOpen(t, dir)
	if built, err := c.Built(); err != nil || !built.Equal(dlStart) {
		t.Fatalf("built %v %v, want the migration time", built, err)
	}
	if n := scalar[int](t, c.readers, "SELECT count(*) FROM facts"); n != 1 {
		t.Fatalf("%d facts", n)
	}
}

// A binary of version 4 refuses a file of version 5, names both versions,
// and leaves the file as it was. Since step 6 the file of version 5 is made
// by steps 1 to 5.
func TestVersion4BinaryRefusesAVersion5File(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	c, err := openWith(Path(dir), steps[:5])
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	path := Path(dir)
	before := dump(t, path, true)
	old, err := openWith(path, steps[:4])
	if err == nil {
		_ = old.Close()
		t.Fatal("a binary of version 4 opened a file of version 5")
	}
	var newer *migrate.NewerError
	if !errors.As(err, &newer) || newer.Found != 5 || newer.Knows != 4 {
		t.Fatalf("error %v (%T), want a NewerError with 5 and 4", err, err)
	}
	if after := dump(t, path, true); after != before {
		t.Fatalf("the refused file changed\nbefore\n%s\nafter\n%s", before, after)
	}
	missing(t, path+".v"+strconv.Itoa(4))
}
