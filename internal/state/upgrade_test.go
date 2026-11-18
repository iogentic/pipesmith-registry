package state

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// A state file written by release 0.1.1 meets a binary that knows one schema
// step more. That binary writes the copy state.sqlite.v1 first, then
// migrates the file and loads its records, and demo-admin still signs in. A
// binary of the released schema then refuses the migrated file, names both
// versions and leaves the file as it is.
func TestReleasedFileMeetsANewerBinary(t *testing.T) {
	dir := dataDirOf(t, "v1.sqlite")
	path := Path(dir)
	released := dump(t, copied(t, path, filepath.Join(t.TempDir(), "released.sqlite")), true)
	newer := append(slices.Clip(steps), migrate.Step{Version: SchemaVersion + 1, Apply: func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE probe (id INTEGER PRIMARY KEY)")
		return err
	}})

	st, err := openWith(path, false, newer)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	sameRecords(t, got, recordsV1())
	if !auth.VerifyPassword(adminPasswordV1, got.Users[0].PasswordHash) {
		t.Fatal("demo-admin does not sign in after the migration")
	}

	copyPath := path + ".v1"
	info, err := os.Stat(copyPath)
	if err != nil {
		t.Fatalf("no copy of schema 1 beside the migrated file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("copy mode %o, want 600: the file holds password hashes", perm)
	}
	if kept := dump(t, copied(t, copyPath, filepath.Join(t.TempDir(), "kept.sqlite")), true); kept != released {
		t.Fatalf("the copy differs from the released file\ncopy\n%s\nreleased\n%s", kept, released)
	}
	migrated := dump(t, path, true)
	newVersion := strconv.Itoa(SchemaVersion + 1)
	if !strings.Contains(migrated, "table probe on probe: CREATE TABLE probe") ||
		!strings.Contains(migrated, `text "schema", int64 `+newVersion) {
		t.Fatalf("the file did not take the step to schema %s:\n%s", newVersion, migrated)
	}

	for name, open := range map[string]func(string) (*Store, error){"Open": Open, "Create": Create} {
		st, err := open(dir)
		if err == nil {
			_ = st.Close()
			t.Fatalf("%s of the released schema opened a file of schema %s", name, newVersion)
		}
		if want := path + " has schema " + newVersion + ", this binary knows schema " + strconv.Itoa(SchemaVersion); err.Error() != want {
			t.Fatalf("%s refused with %q, want %q", name, err, want)
		}
	}
	if after := dump(t, path, true); after != migrated {
		t.Fatalf("the refusal changed the file\n%s", after)
	}
	if _, err := os.Stat(path + ".v" + newVersion); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refusal wrote a copy: %v", err)
	}
}

// The step list ends at SchemaVersion, the schema row of a file this binary
// writes.
func TestStepsEndAtSchemaVersion(t *testing.T) {
	if len(steps) != SchemaVersion || steps[len(steps)-1].Version != SchemaVersion {
		t.Fatalf("%d steps, the last of version %d; SchemaVersion is %d", len(steps), steps[len(steps)-1].Version, SchemaVersion)
	}
}
