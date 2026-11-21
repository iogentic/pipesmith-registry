package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// The fixture testdata/v5.sqlite is a catalog file of schema 5 with invented
// rows, made like v4.sqlite: its generator holds step 5 as a text of its
// own. The flag -write-fixture writes it again:
//
//	tools/go.sh run 'go test ./internal/catalog -run "^TestFixtureV5IsItsGeneratorOutput$" -write-fixture'
const fixtureV5 = "testdata/v5.sqlite"

// schemaV5 is step 5 as leg 6-cleanup wrote it.
var schemaV5 = []string{
	"CREATE TABLE IF NOT EXISTS facts (key TEXT PRIMARY KEY, value INTEGER NOT NULL) WITHOUT ROWID",
}

// rowsV5 are the rows of v4 and the build time of the file.
var rowsV5 = append(slices.Clip(rowsV4), []struct {
	table  string
	values []any
}{
	{"facts", []any{"built_ms", 1700000000000}},
}...)

// generateV5 writes the fixture of schema 5 at path, a new file.
func generateV5(path string) error {
	return generateFixture(path, slices.Concat(schemaV1[1:], schemaV2, schemaV3, schemaV4, schemaV5), 5, rowsV5)
}

// The committed v5.sqlite is what its generator writes.
func TestFixtureV5IsItsGeneratorOutput(t *testing.T) {
	generated := filepath.Join(t.TempDir(), "v5.sqlite")
	if err := generateV5(generated); err != nil {
		t.Fatal(err)
	}
	if *writeFixture {
		data, err := os.ReadFile(generated)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixtureV5, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := copied(t, fixtureV5, filepath.Join(t.TempDir(), "fixture.sqlite"))
	if got, want := dump(t, fixture, true), dump(t, generated, true); got != want {
		t.Fatalf("testdata/v5.sqlite differs from its generator\nfixture\n%s\ngenerator\n%s", got, want)
	}
}

// Steps 1 to 5 of migrations.go make the schema of the fixture of version 5.
func TestStep5MakesTheSchemaOfTheFixture(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "data"))
	c, err := openWith(path, steps[:5])
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	fixture := copied(t, fixtureV5, filepath.Join(t.TempDir(), "fixture.sqlite"))
	if got, want := dump(t, path, false), dump(t, fixture, false); got != want {
		t.Fatalf("steps 1 to 5 differ from the fixture\nsteps\n%s\nfixture\n%s", got, want)
	}
}

// A binary of version 5 refuses a file of version 6, names both versions,
// and leaves the file as it was.
func TestVersion5BinaryRefusesAVersion6File(t *testing.T) {
	if SchemaVersion != 6 {
		t.Fatalf("SchemaVersion %d, want 6", SchemaVersion)
	}
	dir := filepath.Join(t.TempDir(), "data")
	c := mustOpen(t, dir)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	path := Path(dir)
	before := dump(t, path, true)
	old, err := openWith(path, steps[:5])
	if err == nil {
		_ = old.Close()
		t.Fatal("a binary of version 5 opened a file of version 6")
	}
	var newer *migrate.NewerError
	if !errors.As(err, &newer) || newer.Found != 6 || newer.Knows != 5 {
		t.Fatalf("error %v (%T), want a NewerError with 6 and 5", err, err)
	}
	if after := dump(t, path, true); after != before {
		t.Fatalf("the refused file changed\nbefore\n%s\nafter\n%s", before, after)
	}
	missing(t, path+".v"+strconv.Itoa(5))
}
