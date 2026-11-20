package index_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/index"
)

// The directory that Open creates for the index gives no access to other
// users and no write access to the group.
func TestOpenCreatesDirectoryWithNarrowMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	ix, err := index.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o027 != 0 {
		t.Fatalf("index directory mode %o, want no group write and no access for others", perm)
	}
}

// Finding sa-21: the index names every path of every repository, so Open
// creates index.sqlite with mode 0600, and its WAL and SHM side files take
// that mode. OpenRead of a missing file creates it the same way.
func TestOpenCreatesAPrivateFile(t *testing.T) {
	for name, opener := range map[string]func(string) (*index.Index, error){"Open": index.Open, "OpenRead": index.OpenRead} {
		path := filepath.Join(t.TempDir(), "data", "index.sqlite")
		ix, err := opener(path)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, side := range []string{"", "-wal", "-shm"} {
			info, err := os.Stat(path + side)
			if os.IsNotExist(err) && side != "" {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			found++
			if perm := info.Mode().Perm(); perm&0o077 != 0 {
				t.Errorf("%s: index.sqlite%s has mode %04o, want no right for the group or others", name, side, perm)
			}
		}
		if err := ix.Close(); err != nil {
			t.Fatal(err)
		}
		if found < 2 {
			t.Errorf("%s: %d of the index files exist, want the file and a side file", name, found)
		}
	}
}
