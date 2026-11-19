package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// An older catalog file is copied to catalog.sqlite.v<old> before its
// migration when the free space is at least twice its size, as it is in a
// temporary folder of the tests. The copy holds the file as it was.
func TestOlderFileIsCopiedWhenTheSpaceAllows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := copied(t, fixtureV1, Path(dir))
	before := dump(t, path, true)
	c := mustOpen(t, dir)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + ".v1")
	if err != nil {
		t.Fatalf("no copy: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode %v", info.Mode())
	}
	if got := dump(t, path+".v1", true); got != before {
		t.Fatalf("the copy differs from the file before the migration\ncopy\n%s\nbefore\n%s", got, before)
	}
}
